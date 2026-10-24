//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSessionControlForceStopRejectsGenerationChangedDuringTmuxQuery(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	target := startFakeJob(t, sandbox, "force-generation-target", false, "generation interleaving fixture")
	bystander := startFakeJob(t, sandbox, "force-generation-bystander", false, "bystander fixture")
	session, generation := sessionControlStatus(t, target)
	sessionControlStatus(t, bystander)

	launchBytes, err := os.ReadFile(filepath.Join(target.state, "jobs", target.facts.ID, "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var launch launchRecord
	if err := json.Unmarshal(launchBytes, &launch); err != nil || launch.Pane == nil {
		t.Fatalf("read target pane identity: %v", err)
	}
	pane := *launch.Pane
	current, identityErr := currentKernelIdentity(pane.PID, pane.BootID)
	if identityErr != "" || !sameProcessIdentity(&pane, current) {
		t.Fatalf("target pane identity is not current: %s", identityErr)
	}

	interleaving := filepath.Join(sandbox.root, "force-generation-interleaving")
	replacement := "123e4567-e89b-42d3-a456-426614174099"
	writeReplacementSessionStatus(t, target, replacement, filepath.Join(interleaving, "replacement.json"))
	installForceGenerationTmuxWrapper(t, target, interleaving)

	auth := runCLIJSON(t, target.cli, target.env, "_prepare-stop", target.facts.ID, "--session-id", session, "--generation", generation, "--force", "--json")
	var authorization struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(auth, &authorization); err != nil || authorization.Operation != "force_stop" {
		t.Fatalf("force authorization not prepared: %v: %s", err, auth)
	}
	if err := os.WriteFile(filepath.Join(interleaving, "count"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}

	paused := false
	resume := func() {
		if !paused {
			return
		}
		stillOwned, identityErr := currentKernelIdentity(pane.PID, pane.BootID)
		if identityErr == "" && sameProcessIdentity(&pane, stillOwned) {
			if err := unix.Kill(pane.PID, unix.SIGCONT); err != nil {
				t.Errorf("resume exact fixture pane %d: %v", pane.PID, err)
			}
		} else if identityErr != "" {
			t.Errorf("verify fixture pane before resume: %s", identityErr)
		}
		paused = false
	}
	t.Cleanup(resume)
	if err := unix.Kill(pane.PID, unix.SIGSTOP); err != nil {
		t.Fatalf("pause exact fixture pane heartbeat: %v", err)
	}
	paused = true

	cmd := exec.Command(target.cli, "_confirmed-stop", "--json")
	cmd.Env = target.env
	cmd.Stdin = strings.NewReader(string(auth))
	output, confirmErr := cmd.CombinedOutput()
	if commandExitCode(confirmErr) != 1 {
		t.Fatalf("changed-generation force confirmation exit = %d, want refusal (1): %v: %s", commandExitCode(confirmErr), confirmErr, output)
	}
	if !strings.Contains(string(output), "session generation or foreground changed; no signal sent") {
		t.Fatalf("force refusal did not identify the changed target generation: %s", output)
	}
	if !markerExists(filepath.Join(interleaving, "triggered")) {
		t.Fatal("tmux interleaving trigger did not run")
	}
	countBytes, err := os.ReadFile(filepath.Join(interleaving, "count"))
	if err != nil {
		t.Fatalf("read tmux query count: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(countBytes)))
	if err != nil || count < 3 {
		t.Fatalf("generation replacement occurred after %d tmux queries, want the third query", count)
	}

	resume()
	if ended, err := processIdentityEnded(pane); err != nil || ended {
		t.Fatalf("force refusal did not preserve exact target process: ended=%t err=%v", ended, err)
	}
	if facts := doctor(t, target.cli, target); facts.Terminal != "live" {
		t.Fatalf("changed-generation refusal affected target terminal: %+v", facts)
	}
	if facts := doctor(t, bystander.cli, bystander); facts.Terminal != "live" {
		t.Fatalf("force refusal affected bystander terminal: %+v", facts)
	}
}

func writeReplacementSessionStatus(t *testing.T, job *fakeJob, generation, path string) {
	t.Helper()
	statusPath := filepath.Join(job.state, "jobs", job.facts.ID, "session.json")
	body, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	encodedGeneration, err := json.Marshal(generation)
	if err != nil {
		t.Fatal(err)
	}
	status["generation"] = encodedGeneration
	body, err = json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func installForceGenerationTmuxWrapper(t *testing.T, job *fakeJob, evidenceDir string) {
	t.Helper()
	manifestPath := filepath.Join(job.state, "jobs", job.facts.ID, "manifest.json")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	var realTmux string
	if err := json.Unmarshal(manifest["tmux"], &realTmux); err != nil || realTmux == "" {
		t.Fatalf("read manifest tmux executable: %v", err)
	}
	var socket string
	if err := json.Unmarshal(manifest["socket"], &socket); err != nil || socket == "" {
		t.Fatalf("read manifest socket: %v", err)
	}
	statusPath := filepath.Join(job.state, "jobs", job.facts.ID, "session.json")
	paths := map[string]string{
		"realTmux": realTmux, "socket": socket,
		"count":       filepath.Join(evidenceDir, "count"),
		"replacement": filepath.Join(evidenceDir, "replacement.json"),
		"status":      statusPath,
		"marker":      filepath.Join(evidenceDir, "triggered"),
	}
	wrapper := filepath.Join(evidenceDir, "tmux")
	script := fmt.Sprintf(`#!/bin/sh
real_tmux=%s
target_socket=%s
count_path=%s
replacement=%s
status=%s
marker=%s
if [ "$1" = "-S" ] && [ "$2" = "$target_socket" ] && [ "$4" = "display-message" ]; then
  count=0
  if [ -r "$count_path" ]; then IFS= read -r count < "$count_path"; fi
  count=$((count + 1))
  printf '%%s\n' "$count" > "$count_path.tmp"
  mv -f "$count_path.tmp" "$count_path"
  if [ "$count" -ge 3 ]; then
    : > "$marker"
    cp "$replacement" "$status.tmp"
    mv -f "$status.tmp" "$status"
  fi
fi
exec "$real_tmux" "$@"
`, shellQuote(paths["realTmux"]), shellQuote(paths["socket"]), shellQuote(paths["count"]), shellQuote(paths["replacement"]), shellQuote(paths["status"]), shellQuote(paths["marker"]))
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	encodedWrapper, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	manifest["tmux"] = encodedWrapper
	body, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
