package tui

import (
	"testing"

	"agent-coordinator/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
)

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
