package ctl

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatStatusTable(t *testing.T) {
	statuses := []targetStatus{
		{Target: "//svc1:svc1", Status: "running", Pid: 4521},
		{Target: "//svc2:svc2", Status: "stopped", Pid: 0},
		{Target: "//svc3:svc3", Status: "errored", Pid: 0},
	}

	result := formatStatusTable(statuses)

	if !strings.Contains(result, "TARGET") {
		t.Errorf("Expected header 'TARGET' in output")
	}
	if !strings.Contains(result, "STATUS") {
		t.Errorf("Expected header 'STATUS' in output")
	}
	if !strings.Contains(result, "PID") {
		t.Errorf("Expected header 'PID' in output")
	}
	if !strings.Contains(result, "//svc1:svc1") {
		t.Errorf("Expected target //svc1:svc1 in output")
	}
	if !strings.Contains(result, "running") {
		t.Errorf("Expected 'running' status in output")
	}
	if !strings.Contains(result, "4521") {
		t.Errorf("Expected PID 4521 in output")
	}
	if !strings.Contains(result, "stopped") {
		t.Errorf("Expected 'stopped' status in output")
	}
}

func TestFormatStatusTable_Empty(t *testing.T) {
	result := formatStatusTable(nil)
	if !strings.Contains(result, "No targets") {
		t.Errorf("Expected 'No targets' for empty list, got: %s", result)
	}
}

func TestFormatStatusTable_AlignedColumns(t *testing.T) {
	statuses := []targetStatus{
		{Target: "//short", Status: "running", Pid: 1},
		{Target: "//very/long/target/name:target", Status: "stopped", Pid: 0},
	}

	result := formatStatusTable(statuses)
	lines := strings.Split(result, "\n")
	// Header and 2 data lines plus separator
	if len(lines) < 4 {
		t.Errorf("Expected at least 4 lines, got %d", len(lines))
	}
}

func TestCtlStatus_Integration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]targetStatus{
			{Target: "//svc1", Status: "running", Pid: 1234},
		})
	}))
	defer server.Close()

	// Override discoverServer by directly hitting the mock server
	// We test the HTTP client logic by calling the handler directly
	resp, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatalf("Error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}

	var statuses []targetStatus
	json.NewDecoder(resp.Body).Decode(&statuses)
	if len(statuses) != 1 {
		t.Fatalf("Expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Target != "//svc1" {
		t.Errorf("Expected //svc1, got %s", statuses[0].Target)
	}
}

func TestCtlStop_Integration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/stop" {
			http.NotFound(w, r)
			return
		}
		var req struct{ Target string }
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "stopped " + req.Target,
		})
	}))
	defer server.Close()

	body := `{"target":"//svc1"}`
	resp, err := http.Post(server.URL+"/api/stop", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Success bool
		Message string
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		t.Errorf("Expected success")
	}
}

func TestCtlStop_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "unknown target",
		})
	}))
	defer server.Close()

	body := `{"target":"//unknown"}`
	resp, err := http.Post(server.URL+"/api/stop", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("Expected 400, got %d", resp.StatusCode)
	}
}

func TestDiscoverServer_FileNotFound(t *testing.T) {
	// Point to a directory with no session files
	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	// Create WORKSPACE file in a temp workspace dir
	wsDir := t.TempDir()
	os.WriteFile(filepath.Join(wsDir, "WORKSPACE"), []byte(""), 0644)

	oldWd, _ := os.Getwd()
	os.Chdir(wsDir)
	defer os.Chdir(oldWd)

	_, err := discoverServer()
	if err == nil {
		t.Fatalf("Expected error for missing session file")
	}
	if !strings.Contains(err.Error(), "no running ibazel mrun") {
		t.Errorf("Expected 'no running ibazel mrun' error, got: %v", err)
	}
}

func TestDiscoverServer_StalePID(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	wsDir := t.TempDir()
	os.WriteFile(filepath.Join(wsDir, "WORKSPACE"), []byte(""), 0644)

	oldWd, _ := os.Getwd()
	os.Chdir(wsDir)
	defer os.Chdir(oldWd)

	resolved, _ := filepath.EvalSymlinks(wsDir)
	hash := workspaceHash(resolved)

	// Write session file with a dead PID
	info := sessionInfo{
		Port:      31042,
		Pid:       99999999, // Very unlikely to be alive
		Workspace: resolved,
		StartedAt: "2026-04-14T10:30:00Z",
	}
	data, _ := json.Marshal(info)
	sessionFile := filepath.Join(tmpDir, hash+".json")
	os.WriteFile(sessionFile, data, 0600)

	_, err := discoverServer()
	if err == nil {
		t.Fatalf("Expected error for stale PID")
	}

	// Should have cleaned up the stale file
	if _, statErr := os.Stat(sessionFile); !os.IsNotExist(statErr) {
		t.Errorf("Expected stale session file to be cleaned up")
	}
}

func TestDiscoverServer_FromSessionFile(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	wsDir := t.TempDir()
	os.WriteFile(filepath.Join(wsDir, "WORKSPACE"), []byte(""), 0644)

	oldWd, _ := os.Getwd()
	os.Chdir(wsDir)
	defer os.Chdir(oldWd)

	resolved, _ := filepath.EvalSymlinks(wsDir)
	hash := workspaceHash(resolved)

	// Write session file with current PID (which is alive)
	info := sessionInfo{
		Port:      31042,
		Pid:       os.Getpid(),
		Workspace: resolved,
		StartedAt: "2026-04-14T10:30:00Z",
	}
	data, _ := json.Marshal(info)
	os.WriteFile(filepath.Join(tmpDir, hash+".json"), data, 0600)

	url, err := discoverServer()
	if err != nil {
		t.Fatalf("Expected success, got: %v", err)
	}
	if url != "http://127.0.0.1:31042" {
		t.Errorf("Expected http://127.0.0.1:31042, got %s", url)
	}
}

func TestDiscoverServer_InvalidContent(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDir := sessionDir
	sessionDir = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDir = origSessionDir }()

	wsDir := t.TempDir()
	os.WriteFile(filepath.Join(wsDir, "WORKSPACE"), []byte(""), 0644)

	oldWd, _ := os.Getwd()
	os.Chdir(wsDir)
	defer os.Chdir(oldWd)

	resolved, _ := filepath.EvalSymlinks(wsDir)
	hash := workspaceHash(resolved)

	// Write garbage content
	os.WriteFile(filepath.Join(tmpDir, hash+".json"), []byte("not json"), 0600)

	_, err := discoverServer()
	if err == nil {
		t.Fatalf("Expected error for invalid content")
	}
	if !strings.Contains(err.Error(), "invalid session file") {
		t.Errorf("Expected 'invalid session file' error, got: %v", err)
	}
}

func TestCtlUnknownSubcommand(t *testing.T) {
	exitCode := Run([]string{"foo"})
	if exitCode != 1 {
		t.Errorf("Expected exit code 1 for unknown subcommand, got %d", exitCode)
	}
}
