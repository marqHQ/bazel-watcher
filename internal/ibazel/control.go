package ibazel

import (
	"fmt"
	"os"

	"github.com/bazelbuild/bazel-watcher/internal/ibazel/command"
	"github.com/bazelbuild/bazel-watcher/internal/ibazel/log"
)

type ControlAction int

const (
	ActionRestart ControlAction = iota
	ActionStop
	ActionStart
	ActionAdd
	ActionRemove
	ActionStatus
	ActionList
)

type ControlCommand struct {
	Action   ControlAction
	Target   string
	Args     []string
	Response chan ControlResponse
}

type ControlResponse struct {
	Success bool
	Message string
	Data    interface{}
}

type TargetStatus string

const (
	TargetRunning TargetStatus = "running"
	TargetStopped TargetStatus = "stopped"
	TargetErrored TargetStatus = "errored"
)

type TargetState struct {
	Target    string
	Status    TargetStatus
	DebugArgs []string
}

type TargetStatusInfo struct {
	Target string       `json:"target"`
	Status TargetStatus `json:"status"`
	Pid    int          `json:"pid"`
}

func (i *IBazel) handleControlCommand(cmd ControlCommand) {
	switch cmd.Action {
	case ActionStatus:
		i.controlStatus(cmd)
	case ActionList:
		i.controlList(cmd)
	case ActionStop:
		i.controlStop(cmd)
	case ActionRestart:
		i.controlRestart(cmd)
	case ActionStart:
		i.controlStart(cmd)
	case ActionAdd:
		i.controlAdd(cmd)
	case ActionRemove:
		i.controlRemove(cmd)
	default:
		cmd.Response <- ControlResponse{Success: false, Message: "unknown action"}
	}
}

func (i *IBazel) controlStatus(cmd ControlCommand) {
	statuses := make([]TargetStatusInfo, 0, len(i.allTargets))
	for _, target := range i.allTargets {
		info := TargetStatusInfo{Target: target}
		i.cmdsMu.RLock()
		c, inCmds := i.cmds[target]
		i.cmdsMu.RUnlock()
		if inCmds && c != nil && c.IsSubprocessRunning() {
			info.Status = TargetRunning
			info.Pid = i.getCommandPid(target)
		} else if ts, ok := i.targetStates[target]; ok {
			info.Status = ts.Status
		} else {
			info.Status = TargetStopped
		}
		statuses = append(statuses, info)
	}
	cmd.Response <- ControlResponse{Success: true, Data: statuses}
}

func (i *IBazel) controlList(cmd ControlCommand) {
	targets := make([]string, len(i.allTargets))
	copy(targets, i.allTargets)
	cmd.Response <- ControlResponse{Success: true, Data: targets}
}

func (i *IBazel) controlStop(cmd ControlCommand) {
	idx := containsIdx(i.allTargets, cmd.Target)
	if idx == -1 {
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("unknown target: %s", cmd.Target)}
		return
	}

	if ts, ok := i.targetStates[cmd.Target]; ok && ts.Status == TargetStopped {
		cmd.Response <- ControlResponse{Success: true, Message: fmt.Sprintf("%s already stopped", cmd.Target)}
		return
	}

	i.cmdsMu.Lock()
	c, inCmds := i.cmds[cmd.Target]
	if inCmds {
		delete(i.cmds, cmd.Target)
	}
	i.cmdsMu.Unlock()

	if inCmds && c != nil {
		c.Terminate()
	}

	i.targetStates[cmd.Target] = &TargetState{
		Target: cmd.Target,
		Status: TargetStopped,
	}

	log.Logf("[ctl] Stopped %s", cmd.Target)
	cmd.Response <- ControlResponse{Success: true, Message: fmt.Sprintf("stopped %s", cmd.Target)}
}

func (i *IBazel) controlRestart(cmd ControlCommand) {
	idx := containsIdx(i.allTargets, cmd.Target)
	if idx == -1 {
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("unknown target: %s", cmd.Target)}
		return
	}

	// Terminate existing command if running
	i.cmdsMu.Lock()
	c, inCmds := i.cmds[cmd.Target]
	if inCmds {
		delete(i.cmds, cmd.Target)
	}
	i.cmdsMu.Unlock()

	if inCmds && c != nil {
		c.Terminate()
	}

	// Find debug args for this target
	var debugArg []string
	if idx < len(i.allDebugArgs) {
		debugArg = i.allDebugArgs[idx]
	}

	// Build
	_, errBuild := i.build(cmd.Target)
	if errBuild != nil {
		i.targetStates[cmd.Target] = &TargetState{
			Target:    cmd.Target,
			Status:    TargetErrored,
			DebugArgs: debugArg,
		}
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("build failed for %s: %v", cmd.Target, errBuild)}
		return
	}

	// Setup and start
	newCmd := i.setupRun(cmd.Target, debugArg, i.storedArgsLen)
	logFile := openFileForLogs(cmd.Target)

	i.cmdsMu.Lock()
	i.cmds[cmd.Target] = newCmd
	i.logFiles[cmd.Target] = logFile
	i.cmdsMu.Unlock()

	_, err := newCmd.Start(logFile)
	if err != nil {
		i.targetStates[cmd.Target] = &TargetState{
			Target:    cmd.Target,
			Status:    TargetErrored,
			DebugArgs: debugArg,
		}
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("start failed for %s: %v", cmd.Target, err)}
		return
	}

	i.targetStates[cmd.Target] = &TargetState{
		Target:    cmd.Target,
		Status:    TargetRunning,
		DebugArgs: debugArg,
	}

	log.Logf("[ctl] Restarted %s", cmd.Target)
	cmd.Response <- ControlResponse{Success: true, Message: fmt.Sprintf("restarted %s", cmd.Target)}
}

func (i *IBazel) controlStart(cmd ControlCommand) {
	idx := containsIdx(i.allTargets, cmd.Target)
	if idx == -1 {
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("unknown target: %s", cmd.Target)}
		return
	}

	i.cmdsMu.RLock()
	c, inCmds := i.cmds[cmd.Target]
	i.cmdsMu.RUnlock()
	if inCmds && c != nil && c.IsSubprocessRunning() {
		cmd.Response <- ControlResponse{Success: true, Message: fmt.Sprintf("%s already running", cmd.Target)}
		return
	}

	var debugArg []string
	if idx < len(i.allDebugArgs) {
		debugArg = i.allDebugArgs[idx]
	}

	_, errBuild := i.build(cmd.Target)
	if errBuild != nil {
		i.targetStates[cmd.Target] = &TargetState{
			Target:    cmd.Target,
			Status:    TargetErrored,
			DebugArgs: debugArg,
		}
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("build failed for %s: %v", cmd.Target, errBuild)}
		return
	}

	newCmd := i.setupRun(cmd.Target, debugArg, i.storedArgsLen)
	logFile := openFileForLogs(cmd.Target)

	i.cmdsMu.Lock()
	i.cmds[cmd.Target] = newCmd
	i.logFiles[cmd.Target] = logFile
	i.cmdsMu.Unlock()

	_, err := newCmd.Start(logFile)
	if err != nil {
		i.targetStates[cmd.Target] = &TargetState{
			Target:    cmd.Target,
			Status:    TargetErrored,
			DebugArgs: debugArg,
		}
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("start failed for %s: %v", cmd.Target, err)}
		return
	}

	i.targetStates[cmd.Target] = &TargetState{
		Target:    cmd.Target,
		Status:    TargetRunning,
		DebugArgs: debugArg,
	}

	log.Logf("[ctl] Started %s", cmd.Target)
	cmd.Response <- ControlResponse{Success: true, Message: fmt.Sprintf("started %s", cmd.Target)}
}

func (i *IBazel) controlAdd(cmd ControlCommand) {
	if containsIdx(i.allTargets, cmd.Target) != -1 {
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("target already managed: %s", cmd.Target)}
		return
	}

	i.allTargets = append(i.allTargets, cmd.Target)
	i.allDebugArgs = append(i.allDebugArgs, cmd.Args)

	_, errBuild := i.build(cmd.Target)
	if errBuild != nil {
		i.targetStates[cmd.Target] = &TargetState{
			Target:    cmd.Target,
			Status:    TargetErrored,
			DebugArgs: cmd.Args,
		}
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("build failed for %s: %v", cmd.Target, errBuild)}
		return
	}

	newCmd := i.setupRun(cmd.Target, cmd.Args, i.storedArgsLen)
	logFile := openFileForLogs(cmd.Target)

	i.cmdsMu.Lock()
	if i.cmds == nil {
		i.cmds = make(map[string]command.Command)
	}
	i.cmds[cmd.Target] = newCmd
	if i.logFiles == nil {
		i.logFiles = make(map[string]*os.File)
	}
	i.logFiles[cmd.Target] = logFile
	i.cmdsMu.Unlock()

	_, err := newCmd.Start(logFile)
	if err != nil {
		i.targetStates[cmd.Target] = &TargetState{
			Target:    cmd.Target,
			Status:    TargetErrored,
			DebugArgs: cmd.Args,
		}
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("start failed for %s: %v", cmd.Target, err)}
		return
	}

	// Set up file watches for the new target
	i.watchManyFiles(sourceQuery, []string{cmd.Target}, i.sourceFileWatcher, &i.srcDirToWatch)
	i.watchManyFiles(buildQuery, []string{cmd.Target}, i.buildFileWatcher, &i.bldDirToWatch)

	i.targetStates[cmd.Target] = &TargetState{
		Target:    cmd.Target,
		Status:    TargetRunning,
		DebugArgs: cmd.Args,
	}

	log.Logf("[ctl] Added %s", cmd.Target)
	cmd.Response <- ControlResponse{Success: true, Message: fmt.Sprintf("added %s", cmd.Target)}
}

func (i *IBazel) controlRemove(cmd ControlCommand) {
	idx := containsIdx(i.allTargets, cmd.Target)
	if idx == -1 {
		cmd.Response <- ControlResponse{Success: false, Message: fmt.Sprintf("unknown target: %s", cmd.Target)}
		return
	}

	// Terminate if running
	i.cmdsMu.Lock()
	c, inCmds := i.cmds[cmd.Target]
	if inCmds {
		delete(i.cmds, cmd.Target)
	}
	delete(i.logFiles, cmd.Target)
	i.cmdsMu.Unlock()

	if inCmds && c != nil {
		c.Terminate()
	}

	// Remove from tracking
	i.allTargets = deleteIdx(i.allTargets, idx)
	if idx < len(i.allDebugArgs) {
		i.allDebugArgs = append(i.allDebugArgs[:idx], i.allDebugArgs[idx+1:]...)
	}
	delete(i.targetStates, cmd.Target)

	// Clean up dir watch mappings
	for dir, targets := range i.srcDirToWatch {
		if ti := containsIdx(targets, cmd.Target); ti != -1 {
			i.srcDirToWatch[dir] = deleteIdx(targets, ti)
			if len(i.srcDirToWatch[dir]) == 0 {
				delete(i.srcDirToWatch, dir)
			}
		}
	}
	for dir, targets := range i.bldDirToWatch {
		if ti := containsIdx(targets, cmd.Target); ti != -1 {
			i.bldDirToWatch[dir] = deleteIdx(targets, ti)
			if len(i.bldDirToWatch[dir]) == 0 {
				delete(i.bldDirToWatch, dir)
			}
		}
	}

	log.Logf("[ctl] Removed %s", cmd.Target)
	cmd.Response <- ControlResponse{Success: true, Message: fmt.Sprintf("removed %s", cmd.Target)}
}

// getCommandPid returns the PID of the running command for a target, or 0 if not available.
func (i *IBazel) getCommandPid(target string) int {
	// The command interface doesn't expose PID directly.
	// Return 0 as a placeholder — the status endpoint will show 0 for PID.
	return 0
}


