package ibazel

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/bazelbuild/bazel-watcher/internal/bazel"
	mock_bazel "github.com/bazelbuild/bazel-watcher/internal/bazel/testing"
	"github.com/bazelbuild/bazel-watcher/internal/ibazel/command"
	"github.com/bazelbuild/bazel-watcher/internal/ibazel/fswatcher/common"
	"github.com/bazelbuild/bazel-watcher/internal/ibazel/log"
	analysispb "github.com/bazelbuild/bazel-watcher/third_party/bazel/master/src/main/protobuf/analysis"
	blaze_query "github.com/bazelbuild/bazel-watcher/third_party/bazel/master/src/main/protobuf/blaze_query"
)

// noopCommand is a minimal Command implementation that doesn't block on any channels.
// Used in concurrency tests where we only care about data-race safety, not signal flow.
type noopCommand struct {
	running    bool
	terminated bool
}

func (n *noopCommand) Start(_ *os.File) (*bytes.Buffer, error) { n.running = true; return nil, nil }
func (n *noopCommand) Terminate()                               { n.terminated = true; n.running = false }
func (n *noopCommand) Kill()                                    { n.running = false }
func (n *noopCommand) BeforeRebuild()                           {}
func (n *noopCommand) AfterRebuild(_ *os.File) *bytes.Buffer    { return nil }
func (n *noopCommand) IsSubprocessRunning() bool                 { return n.running }
func (n *noopCommand) Pid() int                                  { return 0 }

func newMockCommand() *mockCommand {
	return &mockCommand{
		signalChan:  make(chan syscall.Signal, 10),
		doTermChan:  make(chan struct{}, 1),
		didTermChan: make(chan struct{}, 1),
	}
}

func newControlTestIBazel(t *testing.T) (*IBazel, *mock_bazel.MockBazel) {
	t.Helper()
	mockBazel := &mock_bazel.MockBazel{}
	bazelNew = func() bazel.Bazel {
		return mockBazel
	}

	i, err := New("testing")
	if err != nil {
		t.Fatalf("Error creating IBazel: %s", err)
	}

	// Replace watchers with fakes
	i.buildFileWatcher.Close()
	i.sourceFileWatcher.Close()
	i.buildFileWatcher = &fakeFSNotifyWatcher{
		EventChan: make(chan common.Event, 1),
	}
	i.sourceFileWatcher = &fakeFSNotifyWatcher{
		EventChan: make(chan common.Event, 1),
	}

	// Initialize control fields
	i.controlCh = make(chan ControlCommand, 100)
	i.targetStates = make(map[string]*TargetState)
	i.cmds = make(map[string]command.Command)
	i.logFiles = make(map[string]*os.File)

	return i, mockBazel
}

// Phase 0: Bug fix tests

func TestTerminateAllCmds_ConcurrentAccess(t *testing.T) {
	log.SetTesting(t)

	// This test verifies that concurrent access to i.cmds doesn't cause a data race.
	// We use a simple Command implementation that returns immediately from Terminate().
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	for j := 0; j < 5; j++ {
		target := "//target" + string(rune('A'+j))
		i.cmdsMu.Lock()
		i.cmds[target] = &noopCommand{running: true}
		i.cmdsMu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		i.terminateAllCmds()
	}()

	go func() {
		defer wg.Done()
		i.cmdsMu.Lock()
		i.cmds["//target_new"] = &noopCommand{running: true}
		i.cmdsMu.Unlock()
	}()

	wg.Wait()
}

func TestKillAllCmds_ConcurrentAccess(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	for j := 0; j < 5; j++ {
		target := "//target" + string(rune('A'+j))
		i.cmdsMu.Lock()
		i.cmds[target] = &noopCommand{running: true}
		i.cmdsMu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		i.killAllCmds()
	}()

	go func() {
		defer wg.Done()
		i.cmdsMu.Lock()
		i.cmds["//target_new"] = &noopCommand{running: true}
		i.cmdsMu.Unlock()
	}()

	wg.Wait()
}

func TestSetupRun_DoesNotMutateSharedArgs(t *testing.T) {
	log.SetTesting(t)

	commandDefaultCommand = func(startupArgs []string, bazelArgs []string, target string, args []string) command.Command {
		return newMockCommand()
	}
	defer func() { commandDefaultCommand = oldCommandDefaultCommand }()

	i, mockBazel := newControlTestIBazel(t)
	defer i.Cleanup()

	mockBazel.AddCQueryResponse("//target:a", defaultCQueryResult("//target:a"))

	originalArgs := []string{"--flag1", "--flag2", "--flag3"}
	i.args = make([]string, len(originalArgs))
	copy(i.args, originalArgs)

	i.setupRun("//target:a", []string{"--debug"}, 2)

	// Verify i.args was not mutated
	if len(i.args) != len(originalArgs) {
		t.Errorf("i.args was mutated: got len %d, want %d", len(i.args), len(originalArgs))
	}
	for idx, v := range originalArgs {
		if i.args[idx] != v {
			t.Errorf("i.args[%d] = %q, want %q", idx, i.args[idx], v)
		}
	}
}

func TestSetupRun_DifferentTargetsIndependent(t *testing.T) {
	log.SetTesting(t)

	var capturedArgs [][]string
	commandDefaultCommand = func(startupArgs []string, bazelArgs []string, target string, args []string) command.Command {
		argsCopy := make([]string, len(args))
		copy(argsCopy, args)
		capturedArgs = append(capturedArgs, argsCopy)
		return newMockCommand()
	}
	defer func() { commandDefaultCommand = oldCommandDefaultCommand }()

	i, mockBazel := newControlTestIBazel(t)
	defer i.Cleanup()

	mockBazel.AddCQueryResponse("//target:a", defaultCQueryResult("//target:a"))
	mockBazel.AddCQueryResponse("//target:b", defaultCQueryResult("//target:b"))

	i.args = []string{"--shared1", "--shared2"}

	i.setupRun("//target:a", []string{"--debugA"}, 2)
	i.setupRun("//target:b", []string{"--debugB"}, 2)

	if len(capturedArgs) != 2 {
		t.Fatalf("Expected 2 captured args, got %d", len(capturedArgs))
	}

	// First target should have --debugA
	if len(capturedArgs[0]) < 1 || capturedArgs[0][0] != "--debugA" {
		t.Errorf("Target A args wrong: got %v", capturedArgs[0])
	}

	// Second target should have --debugB
	if len(capturedArgs[1]) < 1 || capturedArgs[1][0] != "--debugB" {
		t.Errorf("Target B args wrong: got %v", capturedArgs[1])
	}
}

// Phase 1: Status and List tests
// Status and list are served from cache, updated by refreshStatusCache().

func TestControlStatus_AllRunning(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//svc1", "//svc2"}
	cmd1 := newMockCommand()
	cmd1.started = true
	cmd2 := newMockCommand()
	cmd2.started = true
	i.cmds["//svc1"] = cmd1
	i.cmds["//svc2"] = cmd2

	i.refreshStatusCache()

	statusCacheMu.RLock()
	cached := statusCache
	statusCacheMu.RUnlock()

	var statuses []TargetStatusInfo
	if err := json.Unmarshal(cached, &statuses); err != nil {
		t.Fatalf("Error unmarshaling cache: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("Expected 2 statuses, got %d", len(statuses))
	}
	for _, s := range statuses {
		if s.Status != TargetRunning {
			t.Errorf("Expected %s to be running, got %s", s.Target, s.Status)
		}
	}
}

func TestControlStatus_MixedStates(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//svc1", "//svc2"}
	cmd1 := newMockCommand()
	cmd1.started = true
	i.cmds["//svc1"] = cmd1
	i.targetStates["//svc2"] = &TargetState{Target: "//svc2", Status: TargetStopped}

	i.refreshStatusCache()

	statusCacheMu.RLock()
	cached := statusCache
	statusCacheMu.RUnlock()

	var statuses []TargetStatusInfo
	json.Unmarshal(cached, &statuses)
	if statuses[0].Status != TargetRunning {
		t.Errorf("svc1: expected running, got %s", statuses[0].Status)
	}
	if statuses[1].Status != TargetStopped {
		t.Errorf("svc2: expected stopped, got %s", statuses[1].Status)
	}
}

func TestControlStatus_Empty(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.refreshStatusCache()

	statusCacheMu.RLock()
	cached := statusCache
	statusCacheMu.RUnlock()

	var statuses []TargetStatusInfo
	json.Unmarshal(cached, &statuses)
	if len(statuses) != 0 {
		t.Errorf("Expected 0 statuses, got %d", len(statuses))
	}
}

func TestControlList(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//a", "//b", "//c"}

	i.refreshListCache()

	listCacheMu.RLock()
	cached := listCache
	listCacheMu.RUnlock()

	var targets []string
	json.Unmarshal(cached, &targets)
	if len(targets) != 3 {
		t.Fatalf("Expected 3 targets, got %d", len(targets))
	}
	if targets[0] != "//a" || targets[1] != "//b" || targets[2] != "//c" {
		t.Errorf("Wrong target order: %v", targets)
	}
}

// Phase 2: Stop and Restart tests

func TestControlStop_RunningTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	cmd := newMockCommand()
	cmd.started = true
	cmd.doTermChan <- struct{}{}
	i.allTargets = []string{"//svc1"}
	i.cmds["//svc1"] = cmd
	i.targetStates["//svc1"] = &TargetState{Target: "//svc1", Status: TargetRunning}

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionStop, Target: "//svc1", Response: resp})
	r := <-resp

	if !r.Success {
		t.Errorf("Expected success, got: %s", r.Message)
	}
	if cmd.terminated != true {
		t.Errorf("Expected command to be terminated")
	}
	if _, ok := i.cmds["//svc1"]; ok {
		t.Errorf("Expected target removed from cmds")
	}
	if i.targetStates["//svc1"].Status != TargetStopped {
		t.Errorf("Expected target status to be stopped")
	}
}

func TestControlStop_AlreadyStopped(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//svc1"}
	i.targetStates["//svc1"] = &TargetState{Target: "//svc1", Status: TargetStopped}

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionStop, Target: "//svc1", Response: resp})
	r := <-resp

	if !r.Success {
		t.Errorf("Expected success for already-stopped target")
	}
}

func TestControlStop_UnknownTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionStop, Target: "//unknown", Response: resp})
	r := <-resp

	if r.Success {
		t.Errorf("Expected failure for unknown target")
	}
}

func TestControlStop_OtherTargetsUnaffected(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	cmdA := newMockCommand()
	cmdA.started = true
	cmdA.doTermChan <- struct{}{}
	cmdB := newMockCommand()
	cmdB.started = true

	i.allTargets = []string{"//a", "//b"}
	i.cmds["//a"] = cmdA
	i.cmds["//b"] = cmdB
	i.targetStates["//a"] = &TargetState{Target: "//a", Status: TargetRunning}
	i.targetStates["//b"] = &TargetState{Target: "//b", Status: TargetRunning}

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionStop, Target: "//a", Response: resp})
	<-resp

	if cmdB.terminated {
		t.Errorf("Target B should not be terminated")
	}
	if _, ok := i.cmds["//b"]; !ok {
		t.Errorf("Target B should still be in cmds")
	}
}

func TestControlRestart_UnknownTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionRestart, Target: "//unknown", Response: resp})
	r := <-resp

	if r.Success {
		t.Errorf("Expected failure for unknown target")
	}
}

// Phase 3: Start, Add, Remove tests

func TestControlStart_AlreadyRunning(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	cmd := newMockCommand()
	cmd.started = true
	i.allTargets = []string{"//svc1"}
	i.cmds["//svc1"] = cmd

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionStart, Target: "//svc1", Response: resp})
	r := <-resp

	if !r.Success {
		t.Errorf("Expected success (already running)")
	}
}

func TestControlStart_UnknownTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionStart, Target: "//unknown", Response: resp})
	r := <-resp

	if r.Success {
		t.Errorf("Expected failure for unknown target")
	}
}

func TestControlAdd_AlreadyExists(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//svc1"}

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionAdd, Target: "//svc1", Response: resp})
	r := <-resp

	if r.Success {
		t.Errorf("Expected failure for duplicate target")
	}
}

func TestControlRemove_RunningTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	cmd := newMockCommand()
	cmd.started = true
	cmd.doTermChan <- struct{}{}
	i.allTargets = []string{"//a", "//b"}
	i.allDebugArgs = [][]string{{}, {}}
	i.cmds["//a"] = cmd
	i.targetStates["//a"] = &TargetState{Target: "//a", Status: TargetRunning}

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionRemove, Target: "//a", Response: resp})
	r := <-resp

	if !r.Success {
		t.Errorf("Expected success, got: %s", r.Message)
	}
	if cmd.terminated != true {
		t.Errorf("Expected command to be terminated")
	}
	if containsIdx(i.allTargets, "//a") != -1 {
		t.Errorf("Expected target removed from allTargets")
	}
	if _, ok := i.targetStates["//a"]; ok {
		t.Errorf("Expected target removed from targetStates")
	}
}

func TestControlRemove_StoppedTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//a"}
	i.allDebugArgs = [][]string{{}}
	i.targetStates["//a"] = &TargetState{Target: "//a", Status: TargetStopped}

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionRemove, Target: "//a", Response: resp})
	r := <-resp

	if !r.Success {
		t.Errorf("Expected success")
	}
	if len(i.allTargets) != 0 {
		t.Errorf("Expected empty allTargets, got %v", i.allTargets)
	}
}

func TestControlRemove_UnknownTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionRemove, Target: "//unknown", Response: resp})
	r := <-resp

	if r.Success {
		t.Errorf("Expected failure for unknown target")
	}
}

func TestControlRemove_LastTarget(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//only"}
	i.allDebugArgs = [][]string{{}}
	i.targetStates["//only"] = &TargetState{Target: "//only", Status: TargetStopped}

	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionRemove, Target: "//only", Response: resp})
	r := <-resp

	if !r.Success {
		t.Errorf("Expected success")
	}
	if len(i.allTargets) != 0 {
		t.Errorf("Expected empty allTargets")
	}
}

// Integration tests

func TestControlCommand_InWaitState(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//svc1"}
	cmd := newMockCommand()
	cmd.started = true
	cmd.doTermChan <- struct{}{}
	i.cmds["//svc1"] = cmd
	i.targetStates["//svc1"] = &TargetState{Target: "//svc1", Status: TargetRunning}
	i.state = WAIT

	// Send a stop command through controlCh
	resp := make(chan ControlResponse, 1)
	go func() {
		i.controlCh <- ControlCommand{Action: ActionStop, Target: "//svc1", Response: resp}
	}()

	// Step through iterationMultiple — the WAIT select should pick up the command
	dummyCmd := func(targets []string, debugArgs [][]string, argsLength int) ([]*bytes.Buffer, error) {
		return nil, nil
	}
	i.iterationMultiple("run", dummyCmd, []string{"//svc1"}, [][]string{{}}, 0)

	r := <-resp
	if !r.Success {
		t.Errorf("Expected stop command to succeed")
	}
	if i.state != WAIT {
		t.Errorf("Expected state to remain WAIT, got %s", i.state)
	}
}

func TestControlStop_ThenSIGINT(t *testing.T) {
	log.SetTesting(t)
	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()

	cmdA := newMockCommand()
	cmdA.started = true
	cmdA.doTermChan <- struct{}{}
	cmdB := newMockCommand()
	cmdB.started = true
	cmdB.doTermChan <- struct{}{}

	i.allTargets = []string{"//a", "//b"}
	i.cmds["//a"] = cmdA
	i.cmds["//b"] = cmdB
	i.targetStates["//a"] = &TargetState{Target: "//a", Status: TargetRunning}
	i.targetStates["//b"] = &TargetState{Target: "//b", Status: TargetRunning}

	// Stop A via control
	resp := make(chan ControlResponse, 1)
	i.handleControlCommand(ControlCommand{Action: ActionStop, Target: "//a", Response: resp})
	<-resp

	// Now terminateAllCmds should only terminate B (A is removed from cmds)
	i.terminateAllCmds()

	if !cmdB.terminated {
		t.Errorf("B should have been terminated by terminateAllCmds")
	}
}

// Helper to create a default CQuery response for tests
func defaultCQueryResult(target string) *analysispb.CqueryResult {
	name := target
	return &analysispb.CqueryResult{
		Results: []*analysispb.ConfiguredTarget{{
			Target: &blaze_query.Target{
				Type: blaze_query.Target_RULE.Enum(),
				Rule: &blaze_query.Rule{
					Name: &name,
					Attribute: []*blaze_query.Attribute{
						{Name: strPtr("name")},
					},
				},
			},
		}},
	}
}

func strPtr(s string) *string {
	return &s
}
