package ibazel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/bazel-watcher/internal/ibazel/log"
)

func newHTTPTestIBazel(t *testing.T) *IBazel {
	t.Helper()
	i, _ := newControlTestIBazel(t)

	// Clear cached status from prior tests.
	statusCacheMu.Lock()
	statusCache = nil
	statusCacheMu.Unlock()

	// Start a goroutine to drain controlCh and handle commands
	go func() {
		for cmd := range i.controlCh {
			i.handleControlCommand(cmd)
		}
	}()

	return i
}

func TestControlServer_StatusEndpoint(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//svc1", "//svc2"}
	cmd1 := newMockCommand()
	cmd1.started = true
	cmd2 := newMockCommand()
	cmd2.started = true
	i.cmds["//svc1"] = cmd1
	i.cmds["//svc2"] = cmd2

	handler := http.HandlerFunc(i.handleHTTPStatus)
	req := httptest.NewRequest("GET", "/api/status", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var statuses []TargetStatusInfo
	if err := json.NewDecoder(rr.Body).Decode(&statuses); err != nil {
		t.Fatalf("Error decoding response: %v", err)
	}
	if len(statuses) != 2 {
		t.Errorf("Expected 2 statuses, got %d", len(statuses))
	}
}

func TestControlServer_ListEndpoint(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	i.allTargets = []string{"//svc1", "//svc2", "//svc3"}

	handler := http.HandlerFunc(i.handleHTTPList)
	req := httptest.NewRequest("GET", "/api/list", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	var targets []string
	if err := json.NewDecoder(rr.Body).Decode(&targets); err != nil {
		t.Fatalf("Error decoding response: %v", err)
	}
	if len(targets) != 3 {
		t.Errorf("Expected 3 targets, got %d", len(targets))
	}
}

func TestControlServer_StopEndpoint(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	cmd := newMockCommand()
	cmd.started = true
	cmd.doTermChan <- struct{}{}
	i.allTargets = []string{"//svc1"}
	i.cmds["//svc1"] = cmd
	i.targetStates["//svc1"] = &TargetState{Target: "//svc1", Status: TargetRunning}

	handler := i.handleHTTPAction(ActionStop)
	body := `{"target":"//svc1"}`
	req := httptest.NewRequest("POST", "/api/stop", strings.NewReader(body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)
	if result["success"] != true {
		t.Errorf("Expected success=true, got %v", result)
	}
}

func TestControlServer_UnknownTarget(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	handler := i.handleHTTPAction(ActionStop)
	body := `{"target":"//nonexistent"}`
	req := httptest.NewRequest("POST", "/api/stop", strings.NewReader(body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rr.Code)
	}
}

func TestControlServer_MissingBody(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	handler := i.handleHTTPAction(ActionRestart)
	req := httptest.NewRequest("POST", "/api/restart", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestControlServer_InvalidJSON(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	handler := i.handleHTTPAction(ActionRestart)
	req := httptest.NewRequest("POST", "/api/restart", strings.NewReader("{invalid json"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rr.Code)
	}
}

func TestControlServer_MissingTarget(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	handler := i.handleHTTPAction(ActionRestart)
	req := httptest.NewRequest("POST", "/api/restart", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for missing target, got %d", rr.Code)
	}
}

func TestControlServer_SessionFileCreated(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	// Use a temp dir for sessions
	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	i.allTargets = []string{"//svc1"}
	i.writeSessionFile(31042)
	defer func() { controlSessionFile = "" }()

	// Check file exists
	files, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Error reading session dir: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("Expected 1 session file, got %d", len(files))
	}

	// Verify contents
	data, _ := os.ReadFile(filepath.Join(tmpDir, files[0].Name()))
	var info sessionInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("Invalid session file JSON: %v", err)
	}
	if info.Port != 31042 {
		t.Errorf("Expected port 31042, got %d", info.Port)
	}
	if info.Pid != os.Getpid() {
		t.Errorf("Expected PID %d, got %d", os.Getpid(), info.Pid)
	}
}

func TestControlServer_SessionFileCleanedUp(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	i.allTargets = []string{"//svc1"}
	i.writeSessionFile(31042)

	// Verify file exists
	files, _ := os.ReadDir(tmpDir)
	if len(files) != 1 {
		t.Fatalf("Expected 1 session file, got %d", len(files))
	}

	// Clean up
	i.cleanupControlServer()

	files, _ = os.ReadDir(tmpDir)
	if len(files) != 0 {
		t.Errorf("Expected session file to be cleaned up, got %d files", len(files))
	}
}

func TestControlServer_SessionFileContainsStartedAt(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	i.allTargets = []string{"//svc1"}
	beforeWrite := time.Now().UTC()
	i.writeSessionFile(31042)
	defer func() { controlSessionFile = "" }()

	files, _ := os.ReadDir(tmpDir)
	data, _ := os.ReadFile(filepath.Join(tmpDir, files[0].Name()))
	var info sessionInfo
	json.Unmarshal(data, &info)

	startedAt, err := time.Parse(time.RFC3339, info.StartedAt)
	if err != nil {
		t.Fatalf("Could not parse started_at: %v", err)
	}
	if startedAt.Before(beforeWrite.Add(-1 * time.Second)) {
		t.Errorf("started_at %v is before write time %v", startedAt, beforeWrite)
	}
}

func TestControlServer_SessionFileAtomicCreation(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	i.allTargets = []string{"//svc1"}
	i.writeSessionFile(31042)
	defer func() { controlSessionFile = "" }()

	// Try to create another session file for the same workspace — should fail silently
	i.writeSessionFile(31043)

	// Should still only have 1 file
	files, _ := os.ReadDir(tmpDir)
	if len(files) != 1 {
		t.Errorf("Expected 1 session file (atomic creation), got %d", len(files))
	}

	// Verify it still has the original port
	data, _ := os.ReadFile(filepath.Join(tmpDir, files[0].Name()))
	var info sessionInfo
	json.Unmarshal(data, &info)
	if info.Port != 31042 {
		t.Errorf("Expected original port 31042, got %d", info.Port)
	}
}

func TestControlServer_AddEndpoint(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	// Test that add endpoint validates duplicate targets
	i.allTargets = []string{"//svc1"}

	handler := http.HandlerFunc(i.handleHTTPAdd)
	body := `{"target":"//svc1"}`
	req := httptest.NewRequest("POST", "/api/add", strings.NewReader(body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for duplicate target, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestControlServer_StatusCacheFallback(t *testing.T) {
	log.SetTesting(t)

	// Clear any cached status from prior tests.
	statusCacheMu.Lock()
	statusCache = nil
	statusCacheMu.Unlock()

	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()
	i.allTargets = []string{"//svc1"}
	handler := http.HandlerFunc(i.handleHTTPStatus)

	// Prime the cache: drain one command so the request succeeds.
	done := make(chan struct{})
	go func() {
		cmd := <-i.controlCh
		i.handleControlCommand(cmd)
		close(done)
	}()
	req := httptest.NewRequest("GET", "/api/status", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	<-done
	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200 to prime cache, got %d: %s", rr.Code, rr.Body.String())
	}

	// Now shorten the timeout and stop draining — simulates a busy main loop.
	origTimeout := controlTimeout
	controlTimeout = 100 * time.Millisecond
	t.Cleanup(func() { controlTimeout = origTimeout })

	req = httptest.NewRequest("GET", "/api/status", nil)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200 from cache fallback, got %d: %s", rr.Code, rr.Body.String())
	}

	var statuses []TargetStatusInfo
	if err := json.NewDecoder(rr.Body).Decode(&statuses); err != nil {
		t.Fatalf("Error decoding cached response: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Target != "//svc1" {
		t.Errorf("Unexpected cached statuses: %+v", statuses)
	}
}

func TestControlServer_StatusNoCacheReturns503(t *testing.T) {
	log.SetTesting(t)

	// Clear cache, shorten timeout.
	statusCacheMu.Lock()
	statusCache = nil
	statusCacheMu.Unlock()
	origTimeout := controlTimeout
	controlTimeout = 100 * time.Millisecond
	t.Cleanup(func() { controlTimeout = origTimeout })

	i, _ := newControlTestIBazel(t)
	defer i.Cleanup()
	i.allTargets = []string{"//svc1"}

	handler := http.HandlerFunc(i.handleHTTPStatus)
	req := httptest.NewRequest("GET", "/api/status", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 with no cache and busy loop, got %d", rr.Code)
	}
}

func TestControlServer_MethodNotAllowed(t *testing.T) {
	log.SetTesting(t)
	i := newHTTPTestIBazel(t)
	defer i.Cleanup()

	handler := http.HandlerFunc(i.handleHTTPStatus)
	req := httptest.NewRequest("POST", "/api/status", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405, got %d", rr.Code)
	}
}

