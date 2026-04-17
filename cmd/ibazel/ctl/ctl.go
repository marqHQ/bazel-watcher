package ctl

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type sessionInfo struct {
	Port      int      `json:"port"`
	Pid       int      `json:"pid"`
	Workspace string   `json:"workspace"`
	Targets   []string `json:"targets"`
	StartedAt string   `json:"started_at"`
}

type targetStatus struct {
	Target string `json:"target"`
	Status string `json:"status"`
	Pid    int    `json:"pid"`
}

func Run(args []string) int {
	if len(args) == 0 {
		// Interactive TUI mode
		url, err := discoverServer()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		return runTUI(url)
	}

	subcommand := args[0]
	subArgs := args[1:]

	switch subcommand {
	case "status":
		return cmdStatus()
	case "list":
		return cmdList()
	case "stop":
		return cmdTargetAction("stop", subArgs)
	case "start":
		return cmdTargetAction("start", subArgs)
	case "restart":
		return cmdTargetAction("restart", subArgs)
	case "add":
		return cmdAdd(subArgs)
	case "remove":
		return cmdTargetAction("remove", subArgs)
	default:
		fmt.Fprintf(os.Stderr, "Unknown ctl subcommand: %s\nUsage: ibazel ctl [status|list|stop|start|restart|add|remove] [target]\n", subcommand)
		return 1
	}
}

func cmdStatus() int {
	url, err := discoverServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	resp, err := http.Get(url + "/api/status")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to ibazel: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		return 1
	}

	var statuses []targetStatus
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	fmt.Print(formatStatusTable(statuses))
	return 0
}

func cmdList() int {
	url, err := discoverServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	resp, err := http.Get(url + "/api/list")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to ibazel: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		return 1
	}

	var targets []string
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	for _, t := range targets {
		fmt.Println(t)
	}
	return 0
}

func cmdTargetAction(action string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ibazel ctl %s <target>\n", action)
		return 1
	}

	url, err := discoverServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	target := args[0]
	body := fmt.Sprintf(`{"target":%q}`, target)
	resp, err := http.Post(url+"/api/"+action, "application/json", strings.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to ibazel: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.Error)
		return 1
	}

	if !result.Success {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.Message)
		return 1
	}

	fmt.Println(result.Message)
	return 0
}

func cmdAdd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ibazel ctl add <target> [--arg=value ...]\n")
		return 1
	}

	url, err := discoverServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	target := args[0]
	var debugArgs []string
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--arg=") {
			debugArgs = append(debugArgs, strings.TrimPrefix(a, "--arg="))
		}
	}

	reqBody := struct {
		Target string   `json:"target"`
		Args   []string `json:"args,omitempty"`
	}{
		Target: target,
		Args:   debugArgs,
	}

	data, _ := json.Marshal(reqBody)
	resp, err := http.Post(url+"/api/add", "application/json", strings.NewReader(string(data)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to ibazel: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	if !result.Success {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.Message)
		return 1
	}

	fmt.Println(result.Message)
	return 0
}

// discoverServer finds the control server URL for the current workspace.
func discoverServer() (string, error) {
	dir, err := sessionDir()
	if err != nil {
		return "", fmt.Errorf("could not determine session directory: %w", err)
	}

	workspacePath, err := findWorkspaceRoot()
	if err != nil {
		return "", fmt.Errorf("could not find workspace root: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		resolved = workspacePath
	}

	hash := workspaceHash(resolved)
	sessionFile := filepath.Join(dir, hash+".json")

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no running ibazel mrun found in this workspace")
		}
		return "", fmt.Errorf("could not read session file: %w", err)
	}

	var info sessionInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("invalid session file: %w", err)
	}

	// Verify PID is still alive
	if !isProcessAlive(info.Pid) {
		// Stale session file — clean up
		os.Remove(sessionFile)
		return "", fmt.Errorf("no running ibazel mrun found in this workspace (stale session cleaned up)")
	}

	// Verify started_at is plausible (defend against PID reuse)
	if info.StartedAt != "" {
		startedAt, err := time.Parse(time.RFC3339, info.StartedAt)
		if err == nil {
			// If the session claims to be started more than 30 days ago, it's suspicious
			if time.Since(startedAt) > 30*24*time.Hour {
				os.Remove(sessionFile)
				return "", fmt.Errorf("no running ibazel mrun found in this workspace (stale session cleaned up)")
			}
		}
	}

	return fmt.Sprintf("http://127.0.0.1:%d", info.Port), nil
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

func findWorkspaceRoot() (string, error) {
	path, err := os.Getwd()
	if err != nil {
		return "", err
	}

	volume := filepath.VolumeName(path)

	for {
		if path == volume+string(filepath.Separator) {
			path = volume
		}

		if s, err := os.Stat(filepath.Join(path, "WORKSPACE")); err == nil {
			if !s.IsDir() && s.Name() == "WORKSPACE" {
				return path, nil
			}
		}

		if _, err := os.Stat(filepath.Join(path, "WORKSPACE.bazel")); err == nil {
			return path, nil
		}

		if path == volume {
			return "", fmt.Errorf("not in a Bazel workspace")
		}

		path = filepath.Dir(path)
	}
}

func isProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func formatStatusTable(statuses []targetStatus) string {
	if len(statuses) == 0 {
		return "No targets\n"
	}

	// Find column widths
	targetWidth := len("TARGET")
	statusWidth := len("STATUS")
	for _, s := range statuses {
		if len(s.Target) > targetWidth {
			targetWidth = len(s.Target)
		}
		if len(s.Status) > statusWidth {
			statusWidth = len(s.Status)
		}
	}

	var sb strings.Builder
	headerFmt := fmt.Sprintf("%%-%ds  %%-%ds  %%s\n", targetWidth, statusWidth)
	sb.WriteString(fmt.Sprintf(headerFmt, "TARGET", "STATUS", "PID"))
	sb.WriteString(strings.Repeat("-", targetWidth+statusWidth+10) + "\n")

	rowFmt := fmt.Sprintf("%%-%ds  %%-%ds  %%s\n", targetWidth, statusWidth)
	for _, s := range statuses {
		pidStr := "-"
		if s.Pid > 0 {
			pidStr = fmt.Sprintf("%d", s.Pid)
		}
		sb.WriteString(fmt.Sprintf(rowFmt, s.Target, s.Status, pidStr))
	}

	return sb.String()
}
