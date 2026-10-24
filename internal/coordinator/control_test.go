package coordinator

import (
	"strings"
	"testing"
	"time"
)

func TestValidateSessionStatusRejectsStaleAndMismatchedSnapshots(t *testing.T) {
	now := time.UnixMilli(20_000)
	valid := SessionStatus{Protocol: ControlProtocol, JobID: "123e4567-e89b-42d3-a456-426614174000", Owner: "owner", SessionID: "native", Generation: "123e4567-e89b-42d3-a456-426614174002", PID: 123, UpdatedAt: now.UnixMilli(), Activity: "busy"}
	if err := validateSessionStatus(valid, valid.JobID, valid.Owner, valid.PID, now); err != nil {
		t.Fatalf("fresh exact status rejected: %v", err)
	}
	for _, mutate := range []struct {
		name string
		edit func(*SessionStatus)
	}{
		{"stale", func(s *SessionStatus) { s.UpdatedAt -= 10_001 }},
		{"wrong job", func(s *SessionStatus) { s.JobID = "123e4567-e89b-42d3-a456-426614174001" }},
		{"wrong owner", func(s *SessionStatus) { s.Owner = "other" }},
		{"wrong pid", func(s *SessionStatus) { s.PID++ }},
		{"unknown activity", func(s *SessionStatus) { s.Activity = "finished" }},
		{"oversized report", func(s *SessionStatus) { s.Report = &AgentReport{Summary: strings.Repeat("x", 1025)} }},
		{"oversized attention", func(s *SessionStatus) {
			value := strings.Repeat("x", 1025)
			s.Report = &AgentReport{Summary: "working", Attention: &value}
		}},
		{"blank attention", func(s *SessionStatus) { value := " \t"; s.Report = &AgentReport{Summary: "working", Attention: &value} }},
		{"empty prompt kind", func(s *SessionStatus) { s.Prompt = &ObservedPrompt{} }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			status := valid
			mutate.edit(&status)
			if err := validateSessionStatus(status, valid.JobID, valid.Owner, valid.PID, now); err == nil {
				t.Fatal("invalid session status was trusted")
			}
		})
	}
}

func TestControlEndpointIsSiblingOfManagedTmuxSocket(t *testing.T) {
	got := controlEndpoint("/tmp/private/job/s")
	if got != "/tmp/private/job/control" {
		t.Fatalf("control endpoint = %q", got)
	}
}

func TestConfirmStopRejectsExpiredAuthorizationBeforeTargetAccess(t *testing.T) {
	coordinator := &Coordinator{}
	auth := StopAuthorization{Token: strings.Repeat("a", 64), Operation: "stop", ExpiresAt: time.Now().Add(-time.Second).UnixMilli()}
	if _, err := coordinator.ConfirmStop(t.Context(), auth); err == nil {
		t.Fatal("expired authorization was accepted")
	}
}
