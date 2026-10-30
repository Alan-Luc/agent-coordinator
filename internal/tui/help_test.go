package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"coordinate/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
)

func TestHelpOverlaysDashboard(t *testing.T) {
	for _, size := range []struct{ width, height int }{{80, 24}, {120, 30}, {20, 8}} {
		m := newModel(t.Context(), nil)
		m.width, m.height = size.width, size.height
		m.jobs = []coordinator.JobFacts{{ID: "job", Name: "worker"}}
		background := strings.Split(ansi.Strip(lipgloss.NewStyle().Width(size.width).Height(size.height).Render(m.View())), "\n")
		m.mode = help
		view := ansi.Strip(m.View())
		lines := strings.Split(view, "\n")
		if !strings.Contains(view, "Help") && !strings.Contains(view, "help") {
			t.Fatal("help panel missing")
		}
		if lines[0] != background[0] || lines[len(lines)-1] != background[len(background)-1] {
			t.Fatalf("help replaced background outside the modal at %dx%d:\n%s", size.width, size.height, view)
		}
	}
}

func TestHelpPreservesWideRuneBackgroundColumns(t *testing.T) {
	row := strings.Repeat("a", 60) + "界TAIL" + strings.Repeat(" ", 14)
	background := strings.Repeat(row+"\n", 23) + row
	view := ansi.Strip(overlayHelp(background, 80, 24))
	for _, line := range strings.Split(view, "\n") {
		index := strings.Index(line, "TAIL")
		if index < 0 || ansi.StringWidth(line[:index]) != 62 || ansi.StringWidth(line) != 80 {
			t.Fatalf("help shifted background columns: %q", line)
		}
	}
}

func TestOpeningHelpInvalidatesPendingControlTarget(t *testing.T) {
	m := model{viewGeneration: 4, jobs: []coordinator.JobFacts{{ID: "job", Name: "worker", Session: &coordinator.SessionStatus{SessionID: "native", Generation: "runtime"}}}}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	m = updated.(model)
	updated, command := m.Update(targetMsg{jobID: "job", generation: 4, action: forceStopAction, target: coordinator.SessionTarget{ID: "job", SessionID: "native", Generation: "runtime"}})
	m = updated.(model)
	if m.mode != help || m.controlTarget != nil || command != nil {
		t.Fatalf("pending control resolution replaced help: mode=%v target=%+v", m.mode, m.controlTarget)
	}
}
