package ibazel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bazelbuild/bazel-watcher/internal/ibazel/log"
)

const (
	controlPortStart      uint16 = 31000
	controlPortEnd        uint16 = 31100
	controlTimeoutDefault        = 30 * time.Second
)

// controlTimeout is a variable so tests can shorten it.
var controlTimeout = controlTimeoutDefault

type sessionInfo struct {
	Port      int      `json:"port"`
	Pid       int      `json:"pid"`
	Workspace string   `json:"workspace"`
	Targets   []string `json:"targets"`
	StartedAt string   `json:"started_at"`
}

var (
	controlServer      *http.Server
	controlSessionDir  string
	controlSessionFile string

	// Read-only caches — updated by the builder, served by the HTTP handlers.
	// This decouples reads from the main build loop so they never block.
	statusCacheMu sync.RWMutex
	statusCache   []byte
	listCacheMu   sync.RWMutex
	listCache     []byte
)

func (i *IBazel) startControlServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", i.handleHTTPStatus)
	mux.HandleFunc("/api/list", i.handleHTTPList)
	mux.HandleFunc("/api/restart", i.handleHTTPAction(ActionRestart))
	mux.HandleFunc("/api/stop", i.handleHTTPAction(ActionStop))
	mux.HandleFunc("/api/start", i.handleHTTPAction(ActionStart))
	mux.HandleFunc("/api/add", i.handleHTTPAdd)
	mux.HandleFunc("/api/remove", i.handleHTTPAction(ActionRemove))

	var listener net.Listener
	var port uint16
	for port = controlPortStart; port < controlPortEnd; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			listener = l
			break
		}
	}
	if listener == nil {
		log.Errorf("Could not find open port for control server in range %d-%d", controlPortStart, controlPortEnd)
		return
	}

	controlServer = &http.Server{
		Handler: mux,
	}

	go func() {
		if err := controlServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Errorf("Control server error: %v", err)
		}
	}()

	// Prime the status cache so ctl can show targets immediately during
	// the initial build, before the main loop reaches the WAIT state.
	i.primeStatusCache()

	// Periodically check if managed processes are still alive and update
	// the cache so the TUI reflects crashes without waiting for a rebuild.
	go i.processHealthCheck()

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	os.Setenv("IBAZEL_CONTROL_URL", url)

	i.writeSessionFile(int(port))

	log.Logf("Control server listening on %s", url)
}

func (i *IBazel) cleanupControlServer() {
	if controlServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		controlServer.Shutdown(ctx)
		controlServer = nil
	}
	if controlSessionFile != "" {
		os.Remove(controlSessionFile)
		controlSessionFile = ""
	}
}

func (i *IBazel) writeSessionFile(port int) {
	dir, err := sessionDir()
	if err != nil {
		log.Errorf("Could not determine session directory: %v", err)
		return
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Errorf("Could not create session directory: %v", err)
		return
	}

	workspacePath := i.resolvedWorkspacePath()
	hash := workspaceHash(workspacePath)
	filePath := filepath.Join(dir, hash+".json")

	info := sessionInfo{
		Port:      port,
		Pid:       os.Getpid(),
		Workspace: workspacePath,
		Targets:   i.allTargets,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		log.Errorf("Could not marshal session info: %v", err)
		return
	}

	// Try atomic creation first to prevent races between two mrun sessions starting simultaneously.
	// If the file already exists, check whether the owning process is still alive — if not, it's
	// a stale session from a previous crash, so remove it and retry.
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil && os.IsExist(err) {
		if existing, readErr := os.ReadFile(filePath); readErr == nil {
			var old sessionInfo
			if json.Unmarshal(existing, &old) == nil && old.Pid > 0 {
				proc, findErr := os.FindProcess(old.Pid)
				if findErr == nil && proc.Signal(syscall.Signal(0)) == nil {
					log.Errorf("Another ibazel mrun is already running for this workspace (pid %d, session file: %s)", old.Pid, filePath)
					return
				}
			}
		}
		log.Logf("Removing stale session file: %s", filePath)
		os.Remove(filePath)
		f, err = os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if err != nil {
		log.Errorf("Could not create session file: %v", err)
		return
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		log.Errorf("Could not write session file: %v", err)
		return
	}

	controlSessionFile = filePath
	controlSessionDir = dir
}

func (i *IBazel) resolvedWorkspacePath() string {
	ws, err := i.workspaceFinder.FindWorkspace()
	if err != nil {
		// Fall back to cwd
		ws, _ = os.Getwd()
	}
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return ws
	}
	return resolved
}

var sessionDir = func() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "ibazel", "sessions"), nil
}

func workspaceHash(workspacePath string) string {
	h := sha256.Sum256([]byte(workspacePath))
	return fmt.Sprintf("%x", h)
}

// HTTP handlers

type targetRequest struct {
	Target string   `json:"target"`
	Args   []string `json:"args,omitempty"`
}

func (i *IBazel) handleHTTPStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	statusCacheMu.RLock()
	cached := statusCache
	statusCacheMu.RUnlock()

	if cached == nil {
		http.Error(w, `{"error":"server starting up"}`, http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(cached)
}

func (i *IBazel) handleHTTPList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	listCacheMu.RLock()
	cached := listCache
	listCacheMu.RUnlock()

	if cached == nil {
		http.Error(w, `{"error":"server starting up"}`, http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(cached)
}

func (i *IBazel) handleHTTPAction(action ControlAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req targetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
			return
		}

		if req.Target == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "target is required"})
			return
		}

		resp := i.sendControlCommand(ControlCommand{
			Action: action,
			Target: req.Target,
		})
		if resp == nil {
			http.Error(w, `{"error":"build in progress, try again shortly"}`, http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": resp.Success,
			"message": resp.Message,
		})
	}
}

func (i *IBazel) handleHTTPAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req targetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}

	if req.Target == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "target is required"})
		return
	}

	resp := i.sendControlCommand(ControlCommand{
		Action: ActionAdd,
		Target: req.Target,
		Args:   req.Args,
	})
	if resp == nil {
		http.Error(w, `{"error":"build in progress, try again shortly"}`, http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !resp.Success {
		w.WriteHeader(http.StatusBadRequest)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": resp.Success,
		"message": resp.Message,
	})
}

func (i *IBazel) sendControlCommand(cmd ControlCommand) *ControlResponse {
	if i.controlCh == nil {
		return nil
	}

	cmd.Response = make(chan ControlResponse, 1)

	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()

	select {
	case i.controlCh <- cmd:
	case <-ctx.Done():
		return nil
	}

	select {
	case resp := <-cmd.Response:
		return &resp
	case <-ctx.Done():
		return nil
	}
}

// refreshStatusCache snapshots the current target statuses into the cache so
// the HTTP handlers always serve instantly without blocking on the main loop.
func (i *IBazel) refreshStatusCache() {
	statuses := make([]TargetStatusInfo, 0, len(i.allTargets))
	for _, target := range i.allTargets {
		info := TargetStatusInfo{Target: target}
		i.cmdsMu.RLock()
		c, inCmds := i.cmds[target]
		i.cmdsMu.RUnlock()
		if inCmds && c != nil && c.IsSubprocessRunning() {
			info.Status = TargetRunning
			info.Pid = c.Pid()
		} else if ts, ok := i.targetStates[target]; ok {
			info.Status = ts.Status
		} else {
			info.Status = TargetStopped
		}
		statuses = append(statuses, info)
	}
	data, _ := json.Marshal(statuses)
	statusCacheMu.Lock()
	statusCache = data
	statusCacheMu.Unlock()

	i.refreshListCache()
}

// refreshListCache updates the cached target list.
func (i *IBazel) refreshListCache() {
	targets := make([]string, len(i.allTargets))
	copy(targets, i.allTargets)
	data, _ := json.Marshal(targets)
	listCacheMu.Lock()
	listCache = data
	listCacheMu.Unlock()
}

// primeStatusCache seeds the caches with "building" for all targets so
// that ibazel ctl can display them immediately during the initial build.
func (i *IBazel) primeStatusCache() {
	statuses := make([]TargetStatusInfo, len(i.allTargets))
	for idx, target := range i.allTargets {
		statuses[idx] = TargetStatusInfo{
			Target: target,
			Status: TargetBuilding,
		}
	}
	data, _ := json.Marshal(statuses)
	statusCacheMu.Lock()
	statusCache = data
	statusCacheMu.Unlock()

	i.refreshListCache()
}

const healthCheckInterval = 3 * time.Second

// processHealthCheck periodically checks if managed processes are still alive.
// When a process that was "running" has exited, it updates targetStates to
// "stopped" and refreshes the cache so clients see the change immediately.
func (i *IBazel) processHealthCheck() {
	for {
		time.Sleep(healthCheckInterval)

		changed := false
		i.cmdsMu.RLock()
		for _, target := range i.allTargets {
			c, inCmds := i.cmds[target]
			if !inCmds || c == nil {
				continue
			}
			ts, hasState := i.targetStates[target]
			if !hasState || ts.Status != TargetRunning {
				continue
			}
			if !c.IsSubprocessRunning() {
				ts.Status = TargetStopped
				changed = true
			}
		}
		i.cmdsMu.RUnlock()

		if changed {
			i.refreshStatusCache()
		}
	}
}

