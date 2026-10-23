//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRealPTYDashboardRestoresTerminalAfterSignalAndAttachFailure(t *testing.T) {
	requireTmux(t)
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	sandbox := buildBinaries(t)
	job := newSharedFakeJob(t, sandbox, "terminal-failures")
	registerSharedCleanup(t, job)
	failAttach := filepath.Join(job.root, "fail-attach")
	proxy := `#!/bin/sh
for arg do
  if [ "$arg" = attach-session ] && [ -f "$TEST_ATTACH_FAIL" ]; then
    exit 37
  fi
done
exec "$TEST_REAL_TMUX" "$@"
`
	if err := os.WriteFile(filepath.Join(job.root, "bin", "tmux"), []byte(proxy), 0700); err != nil {
		t.Fatal(err)
	}
	job.env = append(job.env, "TEST_REAL_TMUX="+realTmux, "TEST_ATTACH_FAIL="+failAttach)
	job.facts = startSharedFakeJob(t, job, "terminal failure fixture")
	waitForCapture(t, job.capturePath)

	t.Run("handled SIGTERM while attached", func(t *testing.T) {
		command, terminal, done, output := startDashboard(t, job.cli, job.env)
		waitForOutput(t, output, job.facts.Name)
		if _, err := terminal.Write([]byte("\r")); err != nil {
			t.Fatal(err)
		}
		if !waitForClient(job.socket, "0") {
			t.Fatal("dashboard did not attach")
		}
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("handled signal left the dashboard blocked in its attached client")
		}
		assertTerminalModesRestored(t, output)
		_ = terminal.Close()
		if facts := doctor(t, job.cli, job); facts.Terminal != "live" {
			t.Fatalf("dashboard signal stopped the independent Pi process: %+v", facts)
		}
	})

	t.Run("ExecProcess child failure", func(t *testing.T) {
		if err := os.WriteFile(failAttach, nil, 0600); err != nil {
			t.Fatal(err)
		}
		_, terminal, done, output := startDashboard(t, job.cli, job.env)
		waitForOutput(t, output, job.facts.Name)
		if _, err := terminal.Write([]byte("\r")); err != nil {
			t.Fatal(err)
		}
		waitForOutput(t, output, "exit status 37")
		if _, err := terminal.Write([]byte("q")); err != nil {
			t.Fatal(err)
		}
		waitForExit(t, done)
		assertTerminalModesRestored(t, output)
		_ = terminal.Close()
		if facts := doctor(t, job.cli, job); facts.Terminal != "live" {
			t.Fatalf("attach failure stopped the independent Pi process: %+v", facts)
		}
	})
}
