package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"agent-coordinator/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
)

func TestTaskFormRendersControlsLiterallyWithoutChangingTask(t *testing.T) {
	const text = "世界\x1b]52;c;cGF5bG9hZA==\a\nsecond line"
	current := newModel(context.Background(), nil)
	current.mode = newTask
	for _, field := range []int{0, 1, 2} {
		current.field = field
		updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true})
		current = updated.(model)
	}
	if current.name != text || current.cwd != text || current.task != text {
		t.Fatal("terminal-control rendering changed the supplied input")
	}
	view := current.View()
	if strings.ContainsAny(view, "\x1b\a") {
		t.Fatal("form emits executable terminal controls from input")
	}
	for _, visible := range []string{"世界", "\\x1b", "\\a", "\nsecond line"} {
		if !strings.Contains(view, visible) {
			t.Errorf("form did not display literal input %q", visible)
		}
	}
}

func TestRejectedUnpublishedStartKeepsDraft(t *testing.T) {
	current := newModel(context.Background(), nil)
	current.mode, current.field = newTask, 2
	current.name, current.cwd, current.task = "invalid name", "/tmp", "draft line one\ndraft line two"
	updated, launch := current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if launch == nil {
		t.Fatal("submit did not request a launch")
	}
	current = updated.(model)
	updated, retry := current.Update(resultMsg{operation: startJob, err: &coordinator.ActionError{
		ExitCode:   2,
		Diagnostic: coordinator.Diagnostic{Code: "invalid_input", Message: "invalid name"},
	}})
	current = updated.(model)
	if retry != nil {
		t.Fatal("rejected start retried automatically")
	}
	if !strings.Contains(current.View(), "New task") || !strings.Contains(current.View(), current.task) {
		t.Fatal("rejected unpublished start hid the editable draft")
	}
}

func TestPublishedStartFailureRefreshesFactsWithoutRetry(t *testing.T) {
	root := t.TempDir()
	backend, err := coordinator.Open(coordinator.Options{StateDir: filepath.Join(root, "state"), PiPath: filepath.Join(root, "missing-pi")})
	if err != nil {
		t.Fatal(err)
	}
	current := newModel(context.Background(), backend)
	current.mode, current.field = newTask, 2
	current.name, current.cwd, current.task = "example", root, "draft"
	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	current = updated.(model)
	updated, refresh := current.Update(resultMsg{operation: startJob, err: &coordinator.ActionError{
		ExitCode:   75,
		Diagnostic: coordinator.Diagnostic{ID: "123e4567-e89b-42d3-a456-426614174000", Code: "uncertain_launch", Message: "inspect list/doctor; do not retry", Uncertain: true},
	}})
	current = updated.(model)
	if strings.Contains(current.View(), "New task") || refresh == nil {
		t.Fatal("published attempt was not handed back to lifecycle inspection")
	}
	result, ok := refresh().(resultMsg)
	if !ok || result.operation != loadJobs {
		t.Fatal("published start failure retried instead of refreshing read-only facts")
	}
}

func TestTaskFormDisplaysKeyboardFocus(t *testing.T) {
	current := newModel(context.Background(), nil)
	current.mode = newTask
	current.field = 2
	if !strings.Contains(current.View(), "> Task:") {
		t.Fatal("active task field has no visible focus indicator")
	}
}
