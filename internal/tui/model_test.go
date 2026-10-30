package tui

import (
	"context"
	"strings"
	"testing"

	"coordinate/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestTaskFormPreservesMultilineBodyAndSubmits(t *testing.T) {
	m := newModel(context.Background(), nil)
	m.mode = newTask
	m.name, m.cwd, m.task = "worker", "/tmp/work", "first line\nsecond line"
	m.field = 2

	updated, command := m.updateForm(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.mode != newTask || !got.busy || command == nil || got.task != m.task {
		t.Fatalf("submit transition = mode %v busy %v command %v", got.mode, got.busy, command != nil)
	}
}

func TestAttachCompletionAndFailureClearBusy(t *testing.T) {
	for _, test := range []struct {
		name string
		msg  tea.Msg
	}{
		{"returned", attachFinishedMsg{}},
		{"attach failed", attachMsg{err: context.DeadlineExceeded}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newModel(context.Background(), nil)
			m.busy = true
			updated, _ := m.Update(test.msg)
			got := updated.(model)
			if got.busy {
				t.Fatal("busy remained set after attachment ended")
			}
			if test.name == "returned" && strings.Contains(got.message, "remains running") {
				t.Fatalf("Return claimed unverified liveness: %q", got.message)
			}
		})
	}
}

func TestEmptySelectionActionsStayUsable(t *testing.T) {
	for _, key := range []string{"d", "enter"} {
		t.Run(key, func(t *testing.T) {
			m := newModel(context.Background(), nil)
			updated, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if updated.(model).busy || command != nil {
				t.Fatalf("empty selection locked the dashboard: busy=%v command=%v", updated.(model).busy, command != nil)
			}
		})
	}
}

func TestActiveSelectionStartsAttach(t *testing.T) {
	m := newModel(context.Background(), nil)
	m.jobs = []coordinator.JobFacts{{ID: "active-uuid", Name: "worker"}}
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !updated.(model).busy || command == nil {
		t.Fatal("active selection did not start an asynchronous attach")
	}
}

func TestEveryDashboardActionUsesCachedUUID(t *testing.T) {
	job := coordinator.JobFacts{ID: "old-uuid", Name: "worker"}
	for _, op := range []operation{inspectJob, continueJob, repairJob, forgetJob} {
		target, byID, allowed := targetFor(job, op)
		if !allowed || !byID || target != job.ID {
			t.Fatalf("%v target = %q byID=%v allowed=%v, want cached UUID", op, target, byID, allowed)
		}
	}
}

func TestNamelessRowsCannotStartAttachOrObserve(t *testing.T) {
	for _, key := range []string{"enter", "o"} {
		t.Run(key, func(t *testing.T) {
			m := newModel(context.Background(), nil)
			m.jobs = []coordinator.JobFacts{{ID: "broken-uuid"}}
			var updated tea.Model
			var command tea.Cmd
			if key == "enter" {
				updated, command = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			} else {
				updated, command = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			}
			got := updated.(model)
			if got.busy || command != nil {
				t.Fatalf("nameless row left dashboard busy: busy=%v command=%v", got.busy, command != nil)
			}
			updated, command = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
			if command == nil {
				t.Fatal("Quit was not available after rejection")
			}
			if _, ok := command().(tea.QuitMsg); !ok {
				t.Fatal("q did not return a quit command")
			}
		})
	}
}

func TestConfirmationIdentifiesLiteralNameAndUUID(t *testing.T) {
	m := newModel(context.Background(), nil)
	m.mode, m.confirm = confirmAction, forgetJob
	m.jobs = []coordinator.JobFacts{{ID: "cached-uuid\x1b", Name: "worker\x1b"}}
	view := terminalStyles.ReplaceAllString(m.View(), "")
	if !strings.Contains(view, "worker\\x1b") || !strings.Contains(view, `cached-uuid\x1b`) || strings.Contains(view, "\x1b") {
		t.Fatalf("confirmation does not safely identify exact row:\n%s", view)
	}
}

func TestControlGenerationRendersUntrustedIdentifiersLiterally(t *testing.T) {
	target := coordinator.SessionTarget{ID: "job", SessionID: "native\x1b", Generation: "runtime\x9b"}
	m := model{mode: confirmControl, jobs: []coordinator.JobFacts{{ID: "job", Name: "worker"}}, controlTarget: &target, width: 100, height: 30}
	view := terminalStyles.ReplaceAllString(m.View(), "")
	if strings.ContainsAny(view, "\x1b\x9b") || !strings.Contains(view, `Native session: native\x1b`) || !strings.Contains(view, `Runtime identity: runtime\x9b`) {
		t.Fatalf("target identity was not rendered literally:\n%s", view)
	}
}

func TestRetiredRowsOnlyAllowForgetByUUID(t *testing.T) {
	job := coordinator.JobFacts{ID: "old-uuid", Name: "worker", Retired: true}
	for _, op := range []operation{inspectJob, continueJob, repairJob} {
		if target, byID, allowed := targetFor(job, op); allowed || target != "" || byID {
			t.Fatalf("retired %v target = %q byID=%v allowed=%v", op, target, byID, allowed)
		}
	}
	target, byID, allowed := targetFor(job, forgetJob)
	if target != job.ID || !byID || !allowed {
		t.Fatalf("retired Forget target = %q byID=%v allowed=%v", target, byID, allowed)
	}
}

func TestRetiredRowCannotAttachAndKeepsDiagnosticsSecondary(t *testing.T) {
	m := newModel(context.Background(), nil)
	const id = "123e4567-e89b-42d3-a456-426614174000"
	m.jobs = []coordinator.JobFacts{{ID: id, Name: "worker", Retired: true}}
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(model).busy || command != nil {
		t.Fatal("retired row started an attach")
	}
	if !strings.Contains(m.View(), "retired") || strings.Contains(m.View(), id) || !strings.Contains(m.View(), "123e4567") {
		t.Fatalf("default row exposes diagnostics or hides retirement:\n%s", m.View())
	}
}

func TestTaskFormAddsSingleSpace(t *testing.T) {
	m := newModel(context.Background(), nil)
	m.mode, m.field = newTask, 2
	updated, _ := m.updateForm(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	if got := updated.(model).task; got != " " {
		t.Fatalf("space input = %q, want one space", got)
	}
}

func TestHelpConsumesKeysWithoutTriggeringDashboardActions(t *testing.T) {
	m := newModel(context.Background(), nil)
	m.jobs = []coordinator.JobFacts{{ID: "job", Name: "worker"}}
	opened, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	got := opened.(model)
	for _, key := range []string{"n", "d", "c", "s", "X"} {
		updated, command := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if command != nil || updated.(model).mode != help {
			t.Fatalf("help key %q escaped or triggered an action", key)
		}
	}
	for _, key := range []string{"esc", "q", "h"} {
		typeCode := tea.KeyRunes
		if key == "esc" {
			typeCode = tea.KeyEsc
		}
		updated, _ := got.Update(tea.KeyMsg{Type: typeCode, Runes: []rune(key)})
		if updated.(model).mode != dashboard {
			t.Fatalf("%s did not close help", key)
		}
	}
	updated, command := got.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if updated.(model).mode != help || command == nil {
		t.Fatal("Ctrl+C should quit without changing the help state")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("Ctrl+C did not return a quit command")
	}
}

func TestHelpFitsTinyAndOrdinaryTerminals(t *testing.T) {
	for _, size := range []struct{ width, height int }{{1, 1}, {20, 8}, {80, 24}, {120, 30}} {
		view := renderHelp(size.width, size.height)
		if lipgloss.Width(view) > size.width || lipgloss.Height(view) > size.height {
			t.Errorf("help exceeds %dx%d: %dx%d", size.width, size.height, lipgloss.Width(view), lipgloss.Height(view))
		}
		if size.width >= 80 {
			plain := ansi.Strip(view)
			for _, binding := range []string{"↑/↓ select session", "Enter open session", "o toggle Observe", "n create new", "m steer", "s request graceful stop", "X force-stop", "d details", "c process Continue", "r repair", "f Forget", "l refresh", "q / Esc / h close", "F12 Return"} {
				if !strings.Contains(plain, binding) {
					t.Errorf("%dx%d help omitted %q:\n%s", size.width, size.height, binding, plain)
				}
			}
			if !strings.Contains(plain, "╭") || !strings.Contains(plain, "╮") || !strings.Contains(plain, "╰") || !strings.Contains(plain, "╯") {
				t.Errorf("%dx%d help lost its full panel border:\n%s", size.width, size.height, plain)
			}
		}
	}
}

func TestOperationErrorsRemainVisible(t *testing.T) {
	m := newModel(context.Background(), nil)
	updated, _ := m.Update(resultMsg{operation: startJob, err: context.DeadlineExceeded})
	got := updated.(model)
	if got.message != context.DeadlineExceeded.Error() {
		t.Fatalf("operation error hidden: %q", got.message)
	}
}

func TestDashboardExplainsUnknownOutcomeAndSharedInput(t *testing.T) {
	m := newModel(context.Background(), nil)
	m.jobs = []coordinator.JobFacts{{Name: "worker", Launch: "submitted", Terminal: "live", PiReadiness: "unverified", TaskOutcome: "unknown", Clients: 1}}
	view := m.View()
	if !strings.Contains(view, "cwd shared") || !strings.Contains(view, "F12 return") || !strings.Contains(view, "readiness/outcome unverified") {
		t.Fatalf("dashboard omitted lifecycle warnings:\n%s", view)
	}
	if lipgloss.Width(m.renderFooter(false)) > 80 {
		t.Fatalf("compact footer wraps at 80 columns: %q", m.renderFooter(false))
	}
}
