package coordinator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFactsKeepReadinessAndOutcomeIndependent(t *testing.T) {
	facts := JobFacts{ID: "job", Name: "example", Launch: "submitted", Terminal: "live", PiReadiness: "unverified", TaskOutcome: "unknown"}
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"launch":"submitted"`, `"terminal":"live"`, `"pi_readiness":"unverified"`, `"task_outcome":"unknown"`} {
		if !strings.Contains(string(encoded), expected) {
			t.Errorf("missing %s", expected)
		}
	}
}
