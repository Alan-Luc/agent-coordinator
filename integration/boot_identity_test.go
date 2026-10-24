//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBootIdentityLegacyClockDriftKeepsLostSocketSessionVisible(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "boot-clock-legacy", false, "clock drift fixture")
	sessionControlStatus(t, job)
	bootAtStart, err := currentKernelBootID()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(job.state, "jobs", job.facts.ID)
	manifestPath, launchPath := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "launch.json")
	originalManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	originalLaunch, err := os.ReadFile(launchPath)
	if err != nil {
		t.Fatal(err)
	}
	var launch launchRecord
	if err := json.Unmarshal(originalLaunch, &launch); err != nil || launch.Server == nil || launch.Pane == nil {
		t.Fatalf("owned birth identities missing: %v", err)
	}
	server, pane := *launch.Server, *launch.Pane
	socketRemoved := false
	defer func() {
		if err := os.WriteFile(manifestPath, originalManifest, 0600); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(launchPath, originalLaunch, 0600); err != nil {
			t.Error(err)
			return
		}
		if !socketRemoved {
			return
		}
		bootNow, err := currentKernelBootID()
		current, identityError := currentKernelIdentity(server.PID, server.BootID)
		if err != nil || bootNow != bootAtStart || identityError != "" || !sameProcessIdentity(&server, current) {
			t.Errorf("cannot repair exact fixture socket safely: %v %s", err, identityError)
			return
		}
		if err := unix.Kill(server.PID, unix.SIGUSR1); err != nil {
			t.Error(err)
			return
		}
		waitForCondition(t, "owned fixture socket repair", func() bool { info, err := os.Lstat(job.socket); return err == nil && info.Mode()&os.ModeSocket != 0 })
	}()
	stamp, _ := json.Marshal("1791325452.900746")
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(originalManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	delete(manifest, "boot_id_scheme")
	manifest["boot_id"] = stamp
	legacyManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(originalLaunch, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"server", "pane"} {
		var identity map[string]json.RawMessage
		if err := json.Unmarshal(legacy[key], &identity); err != nil {
			t.Fatal(err)
		}
		identity["boot_id"] = stamp
		legacy[key], err = json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
	}
	legacyLaunch, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, legacyManifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchPath, legacyLaunch, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(job.socket); err != nil {
		t.Fatal(err)
	}
	socketRemoved = true
	if ended, err := processIdentityEnded(pane); err != nil || ended {
		t.Fatalf("fixture is not independently alive: ended=%t err=%v", ended, err)
	}
	facts := doctor(t, job.cli, job)
	if facts.Launch != "unknown" || facts.Terminal != "unknown" {
		t.Fatalf("live legacy process with lost socket was falsely ended: %+v", facts)
	}
	var listing struct {
		Jobs []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Launch string `json:"launch"`
		} `json:"jobs"`
	}
	body := runCLIJSON(t, job.cli, job.env, "list", "--json")
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Jobs) != 1 || listing.Jobs[0].ID != job.facts.ID || listing.Jobs[0].Name != job.facts.Name || listing.Jobs[0].Launch != "unknown" {
		t.Fatalf("legacy unknown row lost immutable identity: %s", body)
	}
	forget := exec.Command(job.cli, "forget", "--id", job.facts.ID)
	forget.Env = job.env
	if output, err := forget.CombinedOutput(); err == nil {
		t.Fatalf("ambiguous live legacy work was retired: %s", output)
	}
	duplicate := exec.Command(job.cli, "start", job.facts.Name, "--cwd", job.cwd, "--task-file", "-", "--json")
	duplicate.Env, duplicate.Stdin = job.env, strings.NewReader("must not launch")
	if output, err := duplicate.CombinedOutput(); commandExitCode(err) != 3 {
		t.Fatalf("legacy name reservation was lost: %v: %s", err, output)
	}
	for _, record := range []struct {
		path string
		want []byte
	}{{manifestPath, legacyManifest}, {launchPath, legacyLaunch}} {
		after, err := os.ReadFile(record.path)
		if err != nil || !bytes.Equal(after, record.want) {
			t.Fatalf("read/refusal rewrote legacy record %s: %v", record.path, err)
		}
	}
	if ended, err := processIdentityEnded(pane); err != nil || ended {
		t.Fatalf("legacy inspection/refusal affected live process: ended=%t err=%v", ended, err)
	}
}

var kernelBootUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func currentKernelBootID() (string, error) {
	bootID, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	bootID = strings.ToLower(strings.TrimSpace(bootID))
	if !kernelBootUUID.MatchString(bootID) {
		return "", fmt.Errorf("kernel boot-session UUID is malformed")
	}
	return bootID, nil
}
