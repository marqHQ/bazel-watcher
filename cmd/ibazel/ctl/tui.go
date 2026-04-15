package ctl

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const pollInterval = 2 * time.Second

// Styles
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("27"))
	buildingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	runningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	stoppedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	erroredStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	statusBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("229"))
)

type tuiModel struct {
	serverURL string
	targets   []targetStatus
	cursor    int
	filter    string
	filtering bool
	adding    bool
	addInput  string
	message   string
	msgExpiry time.Time
	err       error
	quitting  bool
	width     int
	height    int
}

type statusMsg []targetStatus
type actionMsg struct {
	success bool
	message string
}
type errMsg struct{ err error }
type tickMsg time.Time

func runTUI(serverURL string) int {
	p := tea.NewProgram(
		newTUIModel(serverURL),
		tea.WithAltScreen(),
	)
	m, err := p.Run()
	if err != nil {
		fmt.Printf("Error running TUI: %v\n", err)
		return 1
	}
	if m.(tuiModel).err != nil {
		fmt.Printf("Error: %v\n", m.(tuiModel).err)
		return 1
	}
	return 0
}

func newTUIModel(serverURL string) tuiModel {
	return tuiModel{
		serverURL: serverURL,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(fetchStatus(m.serverURL), tick())
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case statusMsg:
		m.targets = []targetStatus(msg)
		m.err = nil
		return m, nil

	case actionMsg:
		m.message = msg.message
		m.msgExpiry = time.Now().Add(3 * time.Second)
		return m, fetchStatus(m.serverURL)

	case errMsg:
		m.err = msg.err
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{tick()}
		cmds = append(cmds, fetchStatus(m.serverURL))
		if !m.msgExpiry.IsZero() && time.Now().After(m.msgExpiry) {
			m.message = ""
			m.msgExpiry = time.Time{}
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		if m.filtering {
			return m.handleFilterKey(msg)
		}
		if m.adding {
			return m.handleAddKey(msg)
		}
		return m.handleNormalKey(msg)
	}

	return m, nil
}

func (m tuiModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filter = ""
		return m, nil
	case "enter":
		m.filtering = false
		return m, nil
	case "backspace":
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
		}
		return m, nil
	default:
		if len(msg.String()) == 1 {
			m.filter += msg.String()
		}
		return m, nil
	}
}

func (m tuiModel) handleAddKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.adding = false
		m.addInput = ""
		return m, nil
	case "enter":
		target := m.addInput
		m.adding = false
		m.addInput = ""
		if target == "" {
			return m, nil
		}
		m.message = fmt.Sprintf("Adding %s...", target)
		m.msgExpiry = time.Time{}
		return m, doAction(m.serverURL, "add", target)
	case "backspace":
		if len(m.addInput) > 0 {
			m.addInput = m.addInput[:len(m.addInput)-1]
		}
		return m, nil
	default:
		if len(msg.String()) == 1 {
			m.addInput += msg.String()
		}
		return m, nil
	}
}

func (m tuiModel) handleNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredTargets()

	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(filtered)-1 {
			m.cursor++
		}
	case "r":
		if len(filtered) > 0 && m.cursor < len(filtered) {
			t := filtered[m.cursor].Target
			m.message = fmt.Sprintf("Restarting %s...", t)
			m.msgExpiry = time.Time{}
			return m, doAction(m.serverURL, "restart", t)
		}
	case "s":
		if len(filtered) > 0 && m.cursor < len(filtered) {
			t := filtered[m.cursor].Target
			m.message = fmt.Sprintf("Stopping %s...", t)
			m.msgExpiry = time.Time{}
			return m, doAction(m.serverURL, "stop", t)
		}
	case "x":
		if len(filtered) > 0 && m.cursor < len(filtered) {
			t := filtered[m.cursor].Target
			m.message = fmt.Sprintf("Starting %s...", t)
			m.msgExpiry = time.Time{}
			return m, doAction(m.serverURL, "start", t)
		}
	case "d":
		if len(filtered) > 0 && m.cursor < len(filtered) {
			t := filtered[m.cursor].Target
			m.message = fmt.Sprintf("Removing %s...", t)
			m.msgExpiry = time.Time{}
			return m, doAction(m.serverURL, "remove", t)
		}
	case "a":
		m.adding = true
		m.addInput = ""
	case "/":
		m.filtering = true
		m.filter = ""
	}

	return m, nil
}

func (m tuiModel) filteredTargets() []targetStatus {
	if m.filter == "" {
		return m.targets
	}
	var filtered []targetStatus
	for _, t := range m.targets {
		if strings.Contains(strings.ToLower(t.Target), strings.ToLower(m.filter)) {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

func (m tuiModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("ibazel ctl"))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(erroredStyle.Render(fmt.Sprintf("Connection error: %v", m.err)))
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("Retrying..."))
		b.WriteString("\n")
	}

	filtered := m.filteredTargets()

	// Clamp cursor
	if m.cursor >= len(filtered) {
		m.cursor = max(0, len(filtered)-1)
	}

	// Header
	b.WriteString(fmt.Sprintf("  %-50s  %-8s  %s\n", "TARGET", "STATUS", "PID"))
	b.WriteString("  " + strings.Repeat("-", 70) + "\n")

	// Calculate visible window — reserve lines for chrome around the list:
	//   title(1) + blank(1) + error(0-2) + header(2) + blank(1) + footer(1) = ~8 fixed lines
	const chromeLines = 8
	maxVisible := m.height - chromeLines
	if maxVisible < 3 {
		maxVisible = 3
	}

	// Scroll window around cursor
	viewStart := 0
	viewEnd := len(filtered)
	if len(filtered) > maxVisible {
		viewStart = m.cursor - maxVisible/2
		if viewStart < 0 {
			viewStart = 0
		}
		viewEnd = viewStart + maxVisible
		if viewEnd > len(filtered) {
			viewEnd = len(filtered)
			viewStart = viewEnd - maxVisible
		}
	}

	// Target list — overflow arrows shown in the left margin of first/last rows
	for i := viewStart; i < viewEnd; i++ {
		t := filtered[i]
		margin := "  "
		if i == m.cursor {
			margin = "> "
		} else if i == viewStart && viewStart > 0 {
			margin = "▲ "
		} else if i == viewEnd-1 && viewEnd < len(filtered) {
			margin = "▼ "
		}

		statusStr := renderStatus(t.Status)
		pidStr := "-"
		if t.Pid > 0 {
			pidStr = fmt.Sprintf("%d", t.Pid)
		}

		line := fmt.Sprintf("%s%-50s  %s  %s", margin, t.Target, statusStr, pidStr)
		if i == m.cursor {
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	if len(filtered) == 0 && len(m.targets) > 0 {
		b.WriteString(helpStyle.Render("  No targets match filter"))
		b.WriteString("\n")
	}

	// Footer: status/input + help on a single separator line
	b.WriteString("\n")
	if m.filtering {
		b.WriteString(fmt.Sprintf("Filter: %s_", m.filter))
	} else if m.adding {
		b.WriteString(fmt.Sprintf("Add target: %s_", m.addInput))
	} else if m.message != "" {
		b.WriteString(statusBarStyle.Render(m.message))
	} else {
		b.WriteString(helpStyle.Render("r:restart  s:stop  x:start  a:add  d:remove  /:filter  q:quit"))
	}
	b.WriteString("\n")

	return b.String()
}

func renderStatus(status string) string {
	switch status {
	case "building":
		return buildingStyle.Render("building")
	case "running":
		return runningStyle.Render("running")
	case "stopped":
		return stoppedStyle.Render("stopped")
	case "errored":
		return erroredStyle.Render("errored")
	default:
		return status
	}
}

// Commands

func fetchStatus(serverURL string) tea.Cmd {
	return func() tea.Msg {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(serverURL + "/api/status")
		if err != nil {
			return errMsg{err}
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return errMsg{err}
		}

		var statuses []targetStatus
		if err := json.Unmarshal(body, &statuses); err != nil {
			return errMsg{err}
		}

		return statusMsg(statuses)
	}
}

func doAction(serverURL, action, target string) tea.Cmd {
	return func() tea.Msg {
		client := &http.Client{Timeout: 60 * time.Second}
		body := fmt.Sprintf(`{"target":%q}`, target)
		resp, err := client.Post(serverURL+"/api/"+action, "application/json", strings.NewReader(body))
		if err != nil {
			return actionMsg{success: false, message: fmt.Sprintf("Error: %v", err)}
		}
		defer resp.Body.Close()

		var result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return actionMsg{success: false, message: fmt.Sprintf("Error parsing response: %v", err)}
		}

		return actionMsg{success: result.Success, message: result.Message}
	}
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
