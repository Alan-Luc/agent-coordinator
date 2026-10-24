//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionControlForgetRemovesForceStoppedEndpointAndPreservesRecords(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "control-force-endpoint", false, "endpoint cleanup fixture")
	session, generation := sessionControlStatus(t, job)
	dir := filepath.Join(job.state, "jobs", job.facts.ID)
	var manifest struct {
		Socket string `json:"socket"`
	}
	content, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(filepath.Dir(manifest.Socket), "control")
	before, err := os.Lstat(endpoint)
	if err != nil || before.Mode()&os.ModeSocket == 0 {
		t.Fatalf("fixture endpoint unavailable: %v", err)
	}
	var launch launchRecord
	content, err = os.ReadFile(filepath.Join(dir, "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &launch); err != nil || launch.Pane == nil {
		t.Fatalf("fixture birth identity missing: %v", err)
	}
	t.Cleanup(func() {
		after, err := os.Lstat(endpoint)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		ended, err := processIdentityEnded(*launch.Pane)
		if err != nil || !ended || !os.SameFile(before, after) {
			t.Errorf("cannot safely clean exact fixture endpoint: ended=%t err=%v", ended, err)
			return
		}
		if err := os.Remove(endpoint); err != nil {
			t.Error(err)
		}
	})
	auth := runCLIJSON(t, job.cli, job.env, "_prepare-stop", job.facts.ID, "--session-id", session, "--generation", generation, "--force", "--json")
	stop := exec.Command(job.cli, "_confirmed-stop", "--json")
	stop.Env, stop.Stdin = job.env, strings.NewReader(string(auth))
	if output, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("force exact fake-Pi fixture: %v: %s", err, output)
	}
	waitForCondition(t, "positively ended fake Pi", func() bool { return doctor(t, job.cli, job).Launch == "exited" })
	forget := exec.Command(job.cli, "forget", "--id", job.facts.ID)
	forget.Env = job.env
	if output, err := forget.CombinedOutput(); err != nil {
		t.Fatalf("forget exact ended fixture: %v: %s", err, output)
	}
	if _, err := os.Lstat(endpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Forget left the force-stopped controller endpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("Forget deleted immutable record: %v", err)
	}
	var retired struct {
		Phase   string `json:"phase"`
		Retired bool   `json:"retired"`
	}
	body, err := os.ReadFile(filepath.Join(dir, "launch.json"))
	if err != nil || json.Unmarshal(body, &retired) != nil || retired.Phase != "revoked" || !retired.Retired {
		t.Fatalf("UUID fence not retained after endpoint cleanup: %s: %v", body, err)
	}
}
