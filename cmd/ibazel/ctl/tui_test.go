package ctl

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTUI_InitialRender(t *testing.T) {
	m := newTUIModel("http://localhost:31000")
	m.targets = []targetStatus{
		{Target: "//svc1:svc1", Status: "running", Pid: 4521},
		{Target: "//svc2:svc2", Status: "running", Pid: 4522},
		{Target: "//svc3:svc3", Status: "stopped", Pid: 0},
	}

	view := m.View()
	if !strings.Contains(view, "TARGET") {
		t.Errorf("Expected header in view")
	}
	if !strings.Contains(view, "//svc1:svc1") {
		t.Errorf("Expected svc1 in view")
	}
	if !strings.Contains(view, "//svc3:svc3") {
		t.Errorf("Expected svc3 in view")
	}
	if !strings.Contains(view, "r:restart") {
		t.Errorf("Expected hotkey bar in view")
	}
}

func TestTUI_ArrowKeyNavigation(t *testing.T) {
	m := newTUIModel("http://localhost:31000")
	m.targets = []targetStatus{
		{Target: "//svc1", Status: "running"},
		{Target: "//svc2", Status: "running"},
		{Target: "//svc3", Status: "running"},
	}

	if m.cursor != 0 {
		t.Errorf("Initial cursor should be 0")
	}

	// Move down
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	model := m2.(tuiModel)
	if model.cursor != 1 {
		t.Errorf("Expected cursor at 1 after down, got %d", model.cursor)
	}

	// Move down again
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m3.(tuiModel)
	if model.cursor != 2 {
		t.Errorf("Expected cursor at 2 after second down, got %d", model.cursor)
	}

	// Move down at bottom — should stay
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m4.(tuiModel)
	if model.cursor != 2 {
		t.Errorf("Expected cursor to stay at 2 at bottom, got %d", model.cursor)
	}

	// Move up
	m5, _ := model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = m5.(tuiModel)
	if model.cursor != 1 {
		t.Errorf("Expected cursor at 1 after up, got %d", model.cursor)
	}
}

func TestTUI_FilterTargets(t *testing.T) {
	m := newTUIModel("http://localhost:31000")
	m.targets = []targetStatus{
		{Target: "//svc1:svc1", Status: "running"},
		{Target: "//svc2:svc2", Status: "running"},
		{Target: "//other:other", Status: "running"},
	}

	// Enter filter mode
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model := m2.(tuiModel)
	if !model.filtering {
		t.Errorf("Expected filtering mode to be active")
	}

	// Type filter text
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	model = m3.(tuiModel)
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	model = m4.(tuiModel)
	m5, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model = m5.(tuiModel)

	filtered := model.filteredTargets()
	if len(filtered) != 2 {
		t.Errorf("Expected 2 filtered targets (svc1, svc2), got %d", len(filtered))
	}

	// Verify "other" is filtered out
	for _, f := range filtered {
		if f.Target == "//other:other" {
			t.Errorf("Expected //other:other to be filtered out")
		}
	}
}

func TestTUI_FilterClear(t *testing.T) {
	m := newTUIModel("http://localhost:31000")
	m.targets = []targetStatus{
		{Target: "//svc1", Status: "running"},
	}
	m.filtering = true
	m.filter = "xyz"

	// Press Escape
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(tuiModel)

	if model.filtering {
		t.Errorf("Expected filtering to be disabled after Esc")
	}
	if model.filter != "" {
		t.Errorf("Expected filter to be cleared after Esc")
	}
}

func TestTUI_QuitHotkey(t *testing.T) {
	m := newTUIModel("http://localhost:31000")
	m.targets = []targetStatus{
		{Target: "//svc1", Status: "running"},
	}

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	model := m2.(tuiModel)

	if !model.quitting {
		t.Errorf("Expected quitting to be true after 'q'")
	}
	if cmd == nil {
		t.Errorf("Expected quit command")
	}
}

func TestTUI_StatusRefresh(t *testing.T) {
	m := newTUIModel("http://localhost:31000")
	m.targets = []targetStatus{
		{Target: "//svc1", Status: "running"},
	}

	// Simulate status update
	m2, _ := m.Update(statusMsg([]targetStatus{
		{Target: "//svc1", Status: "stopped"},
	}))
	model := m2.(tuiModel)

	if len(model.targets) != 1 {
		t.Fatalf("Expected 1 target")
	}
	if model.targets[0].Status != "stopped" {
		t.Errorf("Expected status to be updated to 'stopped', got %s", model.targets[0].Status)
	}
}

func TestTUI_ActionFeedback(t *testing.T) {
	m := newTUIModel("http://localhost:31000")

	m2, _ := m.Update(actionMsg{success: true, message: "restarted //svc1"})
	model := m2.(tuiModel)

	if model.message != "restarted //svc1" {
		t.Errorf("Expected message 'restarted //svc1', got %q", model.message)
	}
}

func TestTUI_ActionError(t *testing.T) {
	m := newTUIModel("http://localhost:31000")

	m2, _ := m.Update(actionMsg{success: false, message: "Error: unknown target"})
	model := m2.(tuiModel)

	if model.message != "Error: unknown target" {
		t.Errorf("Expected error message, got %q", model.message)
	}
}

func TestTUI_ServerDisconnect(t *testing.T) {
	m := newTUIModel("http://localhost:31000")

	m2, _ := m.Update(errMsg{err: fmt.Errorf("connection refused")})
	model := m2.(tuiModel)

	if model.err == nil {
		t.Errorf("Expected error to be set")
	}

	view := model.View()
	if !strings.Contains(view, "Connection error") {
		t.Errorf("Expected connection error in view")
	}
}
