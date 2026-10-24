package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"agent-coordinator/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	for _, field := range []struct {
		index int
		label string
	}{{0, "Name"}, {1, "Working directory"}, {2, "Task"}} {
		t.Run(field.label, func(t *testing.T) {
			current := newModel(context.Background(), nil)
			current.mode, current.field = newTask, field.index
			if !strings.Contains(current.View(), "> "+field.label+":") {
				t.Fatalf("active %s field has no visible focus indicator:\n%s", field.label, current.View())
			}
		})
	}
}

func TestTaskFormShowsBoundedActiveTailWithoutChangingDraft(t *testing.T) {
	current := newModel(context.Background(), nil)
	current.mode, current.field = newTask, 2
	current.width, current.height = 80, 24
	current.task = strings.Repeat("draft-prefix-", 20) + "visible-tail-token"

	view := current.View()
	if !strings.Contains(view, "> Task:") || !strings.Contains(view, "visible-tail-token") {
		t.Fatalf("focused task field did not show its active tail:\n%s", view)
	}
	if strings.Contains(view, strings.Repeat("draft-prefix-", 20)) {
		t.Fatalf("long active field was not bounded:\n%s", view)
	}
	if current.task != strings.Repeat("draft-prefix-", 20)+"visible-tail-token" {
		t.Fatal("rendering changed the underlying task draft")
	}
	for _, help := range []string{"Enter", "Ctrl+J", "Esc"} {
		if !strings.Contains(view, help) {
			t.Errorf("form help omitted %q:\n%s", help, view)
		}
	}
}

func TestComposeShowsLiteralMultilineAndKeyboardHelp(t *testing.T) {
	target := coordinator.SessionTarget{ID: "job-uuid", SessionID: "native-session", Generation: "runtime-generation"}
	current := newModel(context.Background(), nil)
	current.mode, current.controlTarget = composeMessage, &target
	current.jobs = []coordinator.JobFacts{{ID: target.ID, Name: "worker", Session: &coordinator.SessionStatus{SessionID: target.SessionID, Generation: target.Generation}}}
	current.controlText = "first direction line\nsecond direction line"

	view := current.View()
	if !strings.Contains(view, "first direction line\nsecond direction line") {
		t.Fatalf("multiline compose draft was flattened or hidden:\n%s", view)
	}
	for _, help := range []string{"Enter send literally", "Ctrl+J newline", "Esc cancel"} {
		if !strings.Contains(view, help) {
			t.Errorf("compose help omitted %q:\n%s", help, view)
		}
	}
	if current.controlText != "first direction line\nsecond direction line" {
		t.Fatal("rendering changed the compose draft")
	}
}

func TestConfirmationRetainsIdentityConsequenceAndChoiceAtSupportedSizes(t *testing.T) {
	const uuid = "123e4567-e89b-42d3-a456-426614174000"
	const nativeID = "native-session-identity"
	const runtimeID = "runtime-generation-identity"
	for _, size := range []struct{ width, height int }{{72, 12}, {80, 24}, {100, 30}} {
		for _, confirmation := range []struct {
			name string
			mode mode
			want []string
		}{
			{name: "continue", mode: confirmAction, want: []string{"Continue", "UUID: " + uuid, "Native session: " + nativeID, "Runtime identity: " + runtimeID, "[y/N]", "task success"}},
			{name: "force stop", mode: confirmControl, want: []string{"Force-stop", "UUID: " + uuid, "Native session: " + nativeID, "Runtime identity: " + runtimeID, "SIGKILL may interrupt writes; detached children may survive", "[y/N]"}},
		} {
			t.Run(fmt.Sprintf("%s/%dx%d", confirmation.name, size.width, size.height), func(t *testing.T) {
				job := coordinator.JobFacts{ID: uuid, Name: "review-worker", Session: &coordinator.SessionStatus{SessionID: nativeID, Generation: runtimeID}}
				current := newModel(context.Background(), nil)
				current.mode, current.width, current.height = confirmation.mode, size.width, size.height
				current.jobs = []coordinator.JobFacts{job}
				current.confirm = continueJob
				current.controlForce = true
				current.controlTarget = &coordinator.SessionTarget{ID: uuid, SessionID: nativeID, Generation: runtimeID}
				view := current.View()
				visible := strings.Join(strings.Fields(view), "")
				for _, expected := range confirmation.want {
					if !strings.Contains(visible, strings.Join(strings.Fields(expected), "")) {
						t.Errorf("confirmation at %dx%d omitted %q:\n%s", size.width, size.height, expected, view)
					}
				}
				for _, line := range strings.Split(view, "\n") {
					if lipgloss.Width(line) > size.width {
						t.Errorf("confirmation line exceeds %d terminal cells (%d): %q", size.width, lipgloss.Width(line), line)
					}
				}
			})
		}
	}
}

func TestConfirmationRequestsResizeForUnrenderableIdentity(t *testing.T) {
	longIdentity := strings.Repeat("identity-segment-", 32)
	current := newModel(context.Background(), nil)
	current.mode, current.width, current.height = confirmControl, 72, 12
	current.jobs = []coordinator.JobFacts{{ID: longIdentity, Name: "worker"}}
	current.controlTarget = &coordinator.SessionTarget{ID: longIdentity, SessionID: longIdentity, Generation: longIdentity}
	view := current.View()
	if !strings.Contains(strings.ToLower(view), "resize") {
		t.Fatalf("confirmation did not request resize for an unrenderable target:\n%s", view)
	}
	if lipgloss.Height(view) > current.height {
		t.Fatalf("confirmation exceeds terminal height: %d > %d", lipgloss.Height(view), current.height)
	}
}
