package coordinator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManifestDeclaresStableBootSchemeWithoutRewritingLegacyShape(t *testing.T) {
	encoded, err := json.Marshal(Manifest{BootID: "3e322019-1073-4f52-afd0-abed9a17ce70", BootIDScheme: BootIdentityScheme})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"boot_id_scheme":"darwin-boot-session-uuid"`) {
		t.Fatalf("stable identity scheme missing: %s", encoded)
	}
	legacy, err := json.Marshal(Manifest{BootID: "1791325452.900746"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), `"boot_id_scheme"`) {
		t.Fatal("legacy records must not gain an implicit scheme")
	}
}

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
