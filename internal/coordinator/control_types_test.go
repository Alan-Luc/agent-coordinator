package coordinator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionFactsKeepAgentAssertionsSeparate(t *testing.T) {
	facts := JobFacts{ID: "job", PiReadiness: "unverified", TaskOutcome: "unknown", Session: &SessionStatus{Protocol: ControlProtocol, JobID: "job", Owner: "owner", SessionID: "native", Generation: "runtime", PID: 123, UpdatedAt: 1, Activity: "idle", Report: &AgentReport{Summary: "Tests running", Attention: stringPointer("Choose a strategy")}}}
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"pi_readiness":"unverified"`, `"task_outcome":"unknown"`, `"session_id":"native"`, `"generation":"runtime"`, `"report":{"summary":"Tests running","attention":"Choose a strategy"}`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("missing %s in %s", want, encoded)
		}
	}
	facts.Session = nil
	encoded, err = json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"session"`) {
		t.Fatal("legacy facts must omit unavailable session observation")
	}
}
func stringPointer(s string) *string { return &s }

func TestControlTargetBindsNativeRuntimeAndBirthIdentity(t *testing.T) {
	target := SessionTarget{ID: "job", SessionID: "native", Generation: "runtime", Pane: ProcessIdentity{BootID: "boot", PID: 123, StartSec: 99, StartUsec: 1}}
	encoded, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id":"job"`, `"session_id":"native"`, `"generation":"runtime"`, `"start_sec":99`, `"start_usec":1`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("missing %s", want)
		}
	}
}
