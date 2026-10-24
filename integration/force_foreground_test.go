//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSessionControlForceStopRejectsForegroundChangedDuringFinalQuery(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	target := startFakeJob(t, sandbox, "force-foreground-target", false, "foreground interleaving fixture")
	bystander := startFakeJob(t, sandbox, "force-foreground-bystander", false, "bystander fixture")
	session, generation := sessionControlStatus(t, target)
	sessionControlStatus(t, bystander)
	dir := filepath.Join(target.state, "jobs", target.facts.ID)
	var launch launchRecord
	body, err := os.ReadFile(filepath.Join(dir, "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &launch); err != nil || launch.Pane == nil {
		t.Fatalf("fixture identity missing: %v", err)
	}
	pane := *launch.Pane
	t.Cleanup(func() {
		body, err := os.ReadFile(filepath.Join(dir, "foreground.json"))
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		var child processIdentity
		if err := json.Unmarshal(body, &child); err != nil {
			t.Error(err)
			return
		}
		boot, err := currentKernelBootID()
		if err != nil || boot != child.BootID {
			t.Errorf("cannot verify child boot: %v", err)
			return
		}
		ended, err := processIdentityEnded(child)
		if err != nil {
			t.Error(err)
			return
		}
		if ended {
			return
		}
		current, detail := currentKernelIdentity(child.PID, boot)
		if detail != "" || !sameProcessIdentity(&child, current) {
			t.Errorf("cannot verify owned foreground child: %s", detail)
			return
		}
		if err := unix.Kill(child.PID, unix.SIGTERM); err != nil {
			t.Error(err)
			return
		}
		waitForCondition(t, "owned foreground child exit", func() bool { ended, err := processIdentityEnded(child); return err == nil && ended })
	})
	evidence := filepath.Join(sandbox.root, "foreground-interleaving")
	// Keep generation unchanged: only real terminal foreground ownership moves.
	writeReplacementSessionStatus(t, target, generation, filepath.Join(evidence, "replacement.json"))
	installForceGenerationTmuxWrapper(t, target, evidence)
	wrapper := filepath.Join(evidence, "tmux")
	script, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Socket string `json:"socket"`
	}
	body, err = os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]string{"action": "send", "text": "fixture-foreground-shift", "request_id": "123e4567-e89b-42d3-a456-426614174090"})
	client := fmt.Sprintf("import socket,json; s=socket.socket(socket.AF_UNIX); s.settimeout(3); s.connect(%q); s.sendall(%q.encode()+b'\\n'); r=json.loads(s.makefile().readline()); assert 'receipt' in r,r", filepath.Join(filepath.Dir(manifest.Socket), "control"), string(request))
	needle := "  if [ \"$count\" -ge 3 ]; then\n"
	if strings.Count(string(script), needle) != 1 {
		t.Fatal("final-query hook not found exactly once")
	}
	hook := "  if [ \"$count\" -ge 4 ]; then\n" + fmt.Sprintf("    if [ ! -f \"$marker\" ]; then %s -c %s || exit 1; fi\n", shellQuote(python), shellQuote(client))
	if err := os.WriteFile(wrapper, []byte(strings.Replace(string(script), needle, hook, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	auth := runCLIJSON(t, target.cli, target.env, "_prepare-stop", target.facts.ID, "--session-id", session, "--generation", generation, "--force", "--json")
	if err := os.WriteFile(filepath.Join(evidence, "count"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(target.cli, "_confirmed-stop", "--json")
	cmd.Env, cmd.Stdin = target.env, strings.NewReader(string(auth))
	output, stopErr := cmd.CombinedOutput()
	if !markerExists(filepath.Join(evidence, "triggered")) {
		t.Fatalf("foreground handoff was not calibrated: %v %s", stopErr, output)
	}
	if commandExitCode(stopErr) != 1 || !strings.Contains(string(output), "unsafe_process_group") {
		t.Fatalf("foreground changed during final query but signal was not refused: %v: %s", stopErr, output)
	}
	if ended, err := processIdentityEnded(pane); err != nil || ended {
		t.Fatalf("target did not survive refusal: ended=%t err=%v", ended, err)
	}
	if facts := doctor(t, bystander.cli, bystander); facts.Terminal != "live" {
		t.Fatalf("bystander affected: %+v", facts)
	}
}
