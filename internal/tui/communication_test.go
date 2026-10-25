package tui

import (
	"errors"
	"strings"
	"testing"

	"agent-coordinator/internal/coordinator"
)

func TestOutstandingCommunicationSurvivesEmptyActiveDashboard(t *testing.T) {
	for _, width := range []int{120, 72, 30} {
		m := newModel(nil, nil)
		m.width, m.height = width, 24
		m.communication = coordinator.CommunicationDiagnostics{Pending: 2, Escalated: 1}
		view := m.View()
		if !strings.Contains(view, "Outstanding 2") || !strings.Contains(view, "escalated 1") {
			t.Fatalf("width %d hides ended-job requests: %q", width, view)
		}
	}
}

func TestCommunicationRefreshIsGenerationFencedAndPreservesLastCountsOnError(t *testing.T) {
	m := newModel(nil, nil)
	m.generation = 2
	current := coordinator.CommunicationDiagnostics{Pending: 3, Escalated: 1}
	updated, _ := m.Update(resultMsg{operation: loadJobs, generation: 2, inbox: &current})
	m = updated.(model)
	if m.communication.Pending != 3 || m.inboxError {
		t.Fatalf("fresh diagnostics ignored: %+v", m.communication)
	}
	stale := coordinator.CommunicationDiagnostics{Pending: 0}
	updated, _ = m.Update(resultMsg{operation: loadJobs, generation: 1, inbox: &stale})
	m = updated.(model)
	if m.communication.Pending != 3 {
		t.Fatal("stale refresh cleared outstanding requests")
	}
	updated, _ = m.Update(resultMsg{operation: loadJobs, generation: 2, inboxErr: errors.New("unavailable")})
	m = updated.(model)
	if m.communication.Pending != 3 || !m.inboxError {
		t.Fatal("failed observation must retain last known counts and show unavailable")
	}
}

func TestCommunicationFailureNeverLooksAllClear(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		m := newModel(nil, nil)
		m.inboxError = unavailable
		m.communication.Degraded = !unavailable
		view := m.View()
		if !strings.Contains(view, "Communication") || !strings.Contains(view, "unavailable") && !strings.Contains(view, "degraded") {
			t.Fatalf("communication failure hidden: %q", view)
		}
	}
}
