package coordinator

import (
	"strings"
	"testing"
)

func TestLaunchEnvironmentDropsParentSessionButKeepsNativeConfig(t *testing.T) {
	for _, key := range []string{"AC_INTRUDER", "PI_SESSION_ID", "PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL", "PI_REASONING_LEVEL"} {
		t.Setenv(key, "parent")
	}
	t.Setenv("PI_CODING_AGENT_DIR", "/private/config")
	t.Setenv("TEST_PROVIDER_KEY", "not-a-real-credential")
	got := map[string]string{}
	for _, entry := range launchEnvironment("/private/state", "owner", "job") {
		k, v, _ := strings.Cut(entry, "=")
		got[k] = v
	}
	for _, key := range []string{"AC_INTRUDER", "PI_SESSION_ID", "PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL", "PI_REASONING_LEVEL"} {
		if _, ok := got[key]; ok {
			t.Errorf("parent marker leaked: %s", key)
		}
	}
	if got["AC_STATE_DIR"] != "/private/state" || got["AC_OWNER"] != "owner" || got["AC_JOB"] != "job" {
		t.Fatal("job identity missing")
	}
	if got["PI_CODING_AGENT_DIR"] != "/private/config" || got["TEST_PROVIDER_KEY"] != "not-a-real-credential" {
		t.Fatal("native configuration lost")
	}
}
