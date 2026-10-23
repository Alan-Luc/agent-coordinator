//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRealCLIRefusesForgetForClaimedLivePi(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := startFakeJob(t, sandbox, "claimed-live", false, "claimed live fixture")
	if capture := waitForCapture(t, job.capturePath); capture.Body != taskHeader+"claimed live fixture" {
		t.Fatalf("fake Pi body = %q", capture.Body)
	}
	forget := exec.Command(job.cli, "forget", job.facts.Name)
	forget.Env = job.env
	if output, err := forget.CombinedOutput(); err == nil {
		t.Fatalf("Forget accepted a claimed live job: %s", output)
	}
	facts := doctor(t, job.cli, job)
	if facts.ID != job.facts.ID || facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("live job changed after refused Forget: %+v", facts)
	}
	if _, err := os.Stat(job.capturePath); err != nil {
		t.Fatalf("live fake Pi disappeared after refused Forget: %v", err)
	}
}

func TestRealCLIForgetsVerifiedExitedPane(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := startFakeJob(t, sandbox, "exited-pane", false, "exit fixture")
	waitForCapture(t, job.capturePath)
	var recorded launchRecord
	content, err := os.ReadFile(filepath.Join(job.state, "jobs", job.facts.ID, "launch.json"))
	if err != nil || json.Unmarshal(content, &recorded) != nil || recorded.Server == nil || recorded.Pane == nil {
		t.Fatalf("read exact owned process identities: %v", err)
	}
	if err := os.WriteFile(job.release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		facts := doctor(t, job.cli, job)
		if facts.Launch == "exited" && facts.Terminal == "exited" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	facts := doctor(t, job.cli, job)
	if facts.Launch != "exited" || facts.Terminal != "exited" {
		t.Fatalf("released fake Pi did not become a verified dead pane: %+v", facts)
	}
	forget := exec.Command(job.cli, "forget", job.facts.Name)
	forget.Env = job.env
	if output, err := forget.CombinedOutput(); err != nil {
		t.Fatalf("Forget verified exited pane: %v: %s", err, output)
	}
	launchPath := filepath.Join(job.state, "jobs", job.facts.ID, "launch.json")
	content, err = os.ReadFile(launchPath)
	if err != nil || json.Unmarshal(content, &recorded) != nil || recorded.Phase != "revoked" {
		t.Fatalf("retirement was not durably recorded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(job.state, "jobs", job.facts.ID, "initial-task.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired task file remains: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paneEnded, paneErr := processIdentityEnded(*recorded.Pane)
		serverEnded, serverErr := processIdentityEnded(*recorded.Server)
		_, socketErr := os.Lstat(job.socket)
		if paneErr == nil && serverErr == nil && paneEnded && serverEnded && errors.Is(socketErr, os.ErrNotExist) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Forget did not retire the exact exited pane and its owned server/socket")
}

func TestRealCLIFactsReconcileStaleSocketAfterBothProcessesEnd(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := startFakeJob(t, sandbox, "stale-socket-ended", false, "stale socket fixture")
	waitForCapture(t, job.capturePath)
	var recorded launchRecord
	content, err := os.ReadFile(filepath.Join(job.state, "jobs", job.facts.ID, "launch.json"))
	if err != nil || json.Unmarshal(content, &recorded) != nil || recorded.Server == nil || recorded.Pane == nil {
		t.Fatalf("read exact owned process identities: %v", err)
	}
	if err := os.WriteFile(job.release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	stop := exec.Command(tmuxPath, "-S", job.socket, "-N", "kill-server")
	if output, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("stop exact owned test server: %v: %s", err, output)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		serverEnded, serverErr := processIdentityEnded(*recorded.Server)
		paneEnded, paneErr := processIdentityEnded(*recorded.Pane)
		if serverErr == nil && paneErr == nil && serverEnded && paneEnded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	serverEnded, serverErr := processIdentityEnded(*recorded.Server)
	paneEnded, paneErr := processIdentityEnded(*recorded.Pane)
	if serverErr != nil || paneErr != nil || !serverEnded || !paneEnded {
		t.Fatalf("owned server/pane did not end: server=%v (%v), pane=%v (%v)", serverEnded, serverErr, paneEnded, paneErr)
	}
	if info, err := os.Lstat(job.socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected stale owned socket to remain: %v %v", info, err)
	}
	facts := doctor(t, job.cli, job)
	if facts.Launch != "exited" || facts.Terminal != "exited" {
		t.Fatalf("both-ended stale-socket job was not reconciled as exited: %+v", facts)
	}
	forget := exec.Command(job.cli, "forget", "--id", job.facts.ID)
	forget.Env = job.env
	if output, err := forget.CombinedOutput(); err != nil {
		t.Fatalf("forget both-ended stale-socket job: %v: %s", err, output)
	}
	if _, err := os.Lstat(job.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact stale socket was not removed: %v", err)
	}
}

func TestRealCLIRepairsOnlyAnOwnedMissingSocket(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := startFakeJob(t, sandbox, "socket-repair", false, "repair fixture")
	waitForCapture(t, job.capturePath)
	info, err := os.Lstat(job.socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected owned tmux socket at %s: mode=%v err=%v", job.socket, info, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		t.Fatalf("socket is not owned by the current user: %#v", info.Sys())
	}
	if facts := doctor(t, job.cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("owned process identity was not verified before socket loss: %+v", facts)
	}
	var recorded launchRecord
	content, err := os.ReadFile(filepath.Join(job.state, "jobs", job.facts.ID, "launch.json"))
	if err != nil || json.Unmarshal(content, &recorded) != nil || recorded.Server == nil || recorded.Pane == nil {
		t.Fatalf("read exact recorded server/pane identities before socket removal: %v", err)
	}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		t.Fatalf("read current kernel boot identity: %v", err)
	}
	bootID := strconv.FormatInt(boot.Sec, 10) + "." + strconv.FormatInt(int64(boot.Usec), 10)
	server, serverErr := currentKernelIdentity(recorded.Server.PID, bootID)
	pane, paneErr := currentKernelIdentity(recorded.Pane.PID, bootID)
	if serverErr != "" || paneErr != "" || !sameProcessIdentity(recorded.Server, server) || !sameProcessIdentity(recorded.Pane, pane) {
		t.Fatalf("recorded socket owner identities are not current: server=%v pane=%v", serverErr, paneErr)
	}
	if err := os.Remove(job.socket); err != nil {
		t.Fatalf("remove exact owned test socket: %v", err)
	}
	if facts := doctor(t, job.cli, job); facts.Launch != "unreachable" || facts.Terminal != "unreachable" {
		t.Fatalf("missing socket was not classified as unreachable: %+v", facts)
	}
	_, terminal, done, output := startTerminal(t, job.cli, job.env, "repair-socket", job.facts.Name)
	waitForOutput(t, output, "Type "+job.facts.Name+" to confirm")
	if _, err := terminal.Write([]byte(job.facts.Name + "\n")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, done)
	_ = terminal.Close()
	if facts := doctor(t, job.cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("confirmed repair did not restore verified owned session: %+v", facts)
	}
	if _, err := os.Stat(job.capturePath); err != nil {
		t.Fatalf("repair replaced or lost the Pi process: %v", err)
	}
}

func TestRealCLIRejectsSchemaAndMarkerMismatchWithoutAction(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := startFakeJob(t, sandbox, "mismatch-live", false, "mismatch fixture")
	waitForCapture(t, job.capturePath)
	manifestPath := filepath.Join(job.state, "jobs", job.facts.ID, "manifest.json")
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(manifestPath, original, 0600); err != nil {
			sandbox.preserve = true
			t.Errorf("restore owned manifest fixture: %v", err)
		}
	})
	var manifest map[string]any
	if err := json.Unmarshal(original, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["schema"] = 999
	changed, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(changed, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	listed := runCLIJSON(t, job.cli, job.env, "list", "--json")
	if !strings.Contains(string(listed), `"id":"`+job.facts.ID+`"`) || !strings.Contains(string(listed), `"launch":"unknown"`) {
		t.Fatalf("schema mismatch did not remain visible as unknown: %s", listed)
	}
	forgetByID := exec.Command(job.cli, "forget", "--id", job.facts.ID)
	forgetByID.Env = job.env
	if output, err := forgetByID.CombinedOutput(); err == nil {
		t.Fatalf("Forget acted on schema-mismatched record: %s", output)
	}
	if _, err := os.Stat(job.capturePath); err != nil {
		t.Fatalf("schema mismatch action stopped Pi: %v", err)
	}
	if err := os.WriteFile(manifestPath, original, 0600); err != nil {
		t.Fatal(err)
	}

	var owner struct {
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal(original, &owner); err != nil || owner.Owner == "" {
		t.Fatalf("read recorded marker owner: %v", err)
	}
	if facts := doctor(t, job.cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("owned session not live before marker mismatch: %+v", facts)
	}
	var transport struct {
		Tmux string `json:"tmux"`
	}
	if err := json.Unmarshal(original, &transport); err != nil || transport.Tmux == "" {
		t.Fatalf("read recorded tmux executable: %v", err)
	}
	set := exec.Command(transport.Tmux, "-S", job.socket, "-N", "set-option", "-t", "ac-"+job.facts.ID, "@ac_owner", "foreign-owner")
	if output, err := set.CombinedOutput(); err != nil {
		t.Fatalf("mutate exact owned session marker: %v: %s", err, output)
	}
	if facts := doctor(t, job.cli, job); facts.Launch != "unknown" {
		t.Fatalf("marker mismatch allowed an owned session action: %+v", facts)
	}
	forgetByID = exec.Command(job.cli, "forget", "--id", job.facts.ID)
	forgetByID.Env = job.env
	if output, err := forgetByID.CombinedOutput(); err == nil {
		t.Fatalf("Forget acted on marker-mismatched session: %s", output)
	}
	if _, err := os.Stat(job.capturePath); err != nil {
		t.Fatalf("marker mismatch action stopped Pi: %v", err)
	}
	reset := exec.Command(transport.Tmux, "-S", job.socket, "-N", "set-option", "-t", "ac-"+job.facts.ID, "@ac_owner", owner.Owner)
	if output, err := reset.CombinedOutput(); err != nil {
		t.Fatal("restore exact owned session marker: ", string(output), err)
	}
}
