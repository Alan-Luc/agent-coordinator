//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRealCLIReportsKnownImmediateExitWithoutUncertainLaunch(t *testing.T) {
	requireTmux(t)
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	sandbox := buildBinaries(t)
	job := newSharedFakeJob(t, sandbox, "immediate-exit")
	registerSharedCleanup(t, job)
	if err := os.WriteFile(filepath.Join(job.root, "bin", "pi"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// Delay only the caller's observation; the bootstrap must still claim and exec normally.
	proxy := `#!/bin/sh
if [ "$4" = display-message ] && [ -z "${TMUX_PANE:-}" ]; then
  count=0
  while [ "$count" -lt 100 ]; do
    dead=$("$TEST_REAL_TMUX" -S "$2" -N display-message -p -t "$7" '#{pane_dead}') || exit 44
    [ "$dead" = 1 ] && break
    count=$((count + 1))
    sleep 0.02
  done
fi
exec "$TEST_REAL_TMUX" "$@"
`
	if err := os.WriteFile(filepath.Join(job.root, "bin", "tmux"), []byte(proxy), 0700); err != nil {
		t.Fatal(err)
	}
	job.env = append(job.env, "TEST_REAL_TMUX="+realTmux)
	facts := startSharedFakeJob(t, job, "one-shot fixture")
	if facts.Terminal != "exited" || facts.PiReadiness != "unverified" || facts.TaskOutcome != "unknown" {
		t.Fatalf("known process exit must not imply uncertain launch or task success: %+v", facts)
	}
}
