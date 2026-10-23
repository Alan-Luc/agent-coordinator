//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const taskHeader = "Initial task supplied by agent-coordinator:\n\n"

type launchFacts struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Launch      string `json:"launch"`
	Terminal    string `json:"terminal"`
	PiReadiness string `json:"pi_readiness"`
	TaskOutcome string `json:"task_outcome"`
}

type fakeCapture struct {
	PID      int               `json:"pid"`
	Args     []string          `json:"args"`
	Body     string            `json:"body"`
	Cwd      string            `json:"cwd"`
	Env      map[string]string `json:"env"`
	TaskPath string            `json:"task_path"`
}

type testSandbox struct {
	root           string
	cli            string
	fake           string
	preserve       bool
	diagnosticPath string
}

func buildBinaries(t *testing.T) *testSandbox {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(os.TempDir(), "agent-coordinator-integration-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sandbox := &testSandbox{root: dir, cli: filepath.Join(dir, "agent-coordinator"), fake: filepath.Join(dir, "pi")}
	t.Cleanup(func() {
		if sandbox.preserve {
			t.Logf("preserved owned integration evidence at %s", sandbox.root)
			return
		}
		_ = os.RemoveAll(sandbox.root)
	})
	cli, fake := sandbox.cli, sandbox.fake
	for _, build := range [][]string{
		{"build", "-o", cli, "./cmd/agent-coordinator"},
		{"build", "-o", fake, "./internal/testpi"},
	} {
		cmd := exec.Command("go", build...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", build[len(build)-1], err, output)
		}
	}
	return sandbox
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrNotExist
	}
	return filepath.Dir(filepath.Dir(file)), nil
}

func requireTmux(t *testing.T) {
	t.Helper()
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux prerequisite unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmux, "-V").Output()
	if err != nil || !strings.Contains(string(out), "tmux 3.") {
		t.Skipf("tmux prerequisite unavailable: %q %v", out, err)
	}
}

func TestRealCLIStartsFakePiAndPreservesPrivateTask(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	cli := sandbox.cli
	task := "\xef\xbb\xbfline one\n世界 @mention /path -flag $(touch SHOULD_NOT_EXIST) </file>\n"
	job := startFakeJob(t, sandbox, "integration-happy", false, task)
	facts := job.facts
	cwd, state, capturePath := job.cwd, job.state, job.capturePath
	if facts.ID == "" || facts.Name != "integration-happy" || facts.Launch != "submitted" || facts.Terminal != "live" || facts.PiReadiness != "unverified" || facts.TaskOutcome != "unknown" {
		t.Fatalf("unexpected launch facts: %+v", facts)
	}
	capture := waitForCapture(t, capturePath)
	if capture.Body != taskHeader+task {
		t.Fatalf("fake Pi received altered task body")
	}
	if capture.Cwd != cwd {
		t.Fatalf("fake Pi cwd = %q, want %q", capture.Cwd, cwd)
	}
	joinedArgs := strings.Join(capture.Args, "\x00")
	if strings.Contains(joinedArgs, task) || !strings.Contains(joinedArgs, "@"+capture.TaskPath) {
		t.Fatalf("task text was not confined to the task file: %q", joinedArgs)
	}
	for key, want := range map[string]string{"AC_STATE_DIR": state, "AC_JOB": facts.ID, "PI_CODING_AGENT_DIR": "/private/test-pi-config", "TEST_PROVIDER_KEY": "private-test-marker"} {
		if got := capture.Env[key]; got != want {
			t.Errorf("fake Pi env %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"AC_INTRUDER", "PI_SESSION_ID", "PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL", "PI_REASONING_LEVEL"} {
		if _, present := capture.Env[key]; present {
			t.Errorf("parent environment marker leaked to fake Pi: %s", key)
		}
	}
	for _, path := range []string{capturePath, filepath.Join(state, "owner.json"), filepath.Join(state, "registry.lock"), filepath.Join(state, "jobs", facts.ID, "manifest.json"), filepath.Join(state, "jobs", facts.ID, "launch.json"), capture.TaskPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat private artifact %s: %v", path, err)
		} else if info.Mode().Perm() != 0600 {
			t.Errorf("private file %s mode = %04o, want 0600", path, info.Mode().Perm())
		}
	}
	manifest, err := os.ReadFile(filepath.Join(state, "jobs", facts.ID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifest, []byte(task)) {
		t.Fatal("task text leaked into routine manifest")
	}
	if _, err := os.Stat(filepath.Join(cwd, "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
		t.Fatalf("task shell syntax executed: stat error %v", err)
	}

	duplicate := exec.Command(cli, "start", "integration-happy", "--cwd", cwd, "--task-file", "-", "--json")
	duplicate.Stdin = strings.NewReader("second task")
	duplicate.Env = job.env
	if output, err := duplicate.CombinedOutput(); err == nil {
		t.Fatalf("duplicate launch unexpectedly succeeded: %s", output)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatalf("duplicate launch exit = %v, output %s; want 3", err, output)
	}
	if _, err := os.Stat(capturePath); err != nil {
		t.Fatalf("original fake Pi capture disappeared: %v", err)
	}
}

func integrationEnv(cli, fake, state, capture, release string) []string {
	path := filepath.Dir(fake) + string(os.PathListSeparator) + os.Getenv("PATH")
	return []string{
		"PATH=" + path,
		"HOME=" + os.Getenv("HOME"),
		"AC_STATE_DIR=" + state,
		"AC_INTRUDER=must-be-stripped",
		"PI_SESSION_ID=parent-session",
		"PI_SESSION_FILE=/private/parent-session.jsonl",
		"PI_PROVIDER=parent-provider",
		"PI_MODEL=parent-model",
		"PI_REASONING_LEVEL=parent-reasoning",
		"PI_CODING_AGENT_DIR=/private/test-pi-config",
		"TEST_PROVIDER_KEY=private-test-marker",
		"TESTPI_CAPTURE=" + capture,
		"TESTPI_INPUT_CAPTURE=" + capture + ".input",
		"TESTPI_INPUT_READY=" + capture + ".input-ready",
		"TESTPI_PAUSED=" + capture + ".paused",
		"TESTPI_RESTORED=" + capture + ".restored",
		"TESTPI_RELEASE=" + release,
		"TESTPI_EXIT=0",
		"TERM=xterm-256color",
		"AGENT_COORDINATOR_EXECUTABLE=" + cli,
	}
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	content, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, content, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestRealPTYEnterObserveAndF12KeepPiRunning(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	cli := sandbox.cli
	job := startFakeJob(t, sandbox, "pty-lifecycle", false, "synthetic integration task")

	enter, enterPTY, enterDone, enterOutput := startTerminal(t, cli, job.env, "enter", job.facts.Name)
	if !waitForClient(job.socket, "0") {
		t.Fatalf("writable tmux client did not appear; enter output=%q", enterOutput.String())
	}
	if _, err := enterPTY.Write([]byte("WRITER_INPUT_REACH_PI\n")); err != nil {
		t.Fatal(err)
	}
	waitForInput(t, job.inputPath, "WRITER_INPUT_REACH_PI")
	if _, err := enterPTY.Write([]byte("\x1b[24~")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, enterDone)
	assertTerminalModesRestored(t, enterOutput)
	_ = enterPTY.Close()
	if facts := doctor(t, cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("F12 stopped the Pi session: %+v", facts)
	}
	if !strings.Contains(enterOutput.String(), "\x1b[?1049l\x1b[?25h\x1b[0m") {
		t.Fatalf("Enter did not restore the outer terminal screen: %q", enterOutput.String())
	}
	_ = enter

	_, observePTY, observeDone, output := startTerminal(t, cli, job.env, "observe", job.facts.Name)
	if !waitForClient(job.socket, "1") {
		t.Fatalf("observer tmux client did not appear; output=%q", output.String())
	}
	if _, err := observePTY.Write([]byte("OBSERVER_INPUT_MUST_NOT_REACH_PI\n\x1b[200~PASTED_OBSERVER_INPUT\x1b[201~\x1b[<0;1;1M")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	input, err := os.ReadFile(job.inputPath)
	if err != nil || bytes.Contains(input, []byte("OBSERVER_INPUT_MUST_NOT_REACH_PI")) || bytes.Contains(input, []byte("PASTED_OBSERVER_INPUT")) {
		t.Fatalf("read-only observer input reached Pi: %q, %v", input, err)
	}
	if _, err := observePTY.Write([]byte("\x1b[24~")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, observeDone)
	assertTerminalModesRestored(t, output)
	_ = observePTY.Close()
	if !strings.Contains(output.String(), "Shared input") {
		t.Fatalf("Enter/Observe warning missing from terminal output: %q", output.String())
	}
	if facts := doctor(t, cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("F12 Return did not preserve the job: %+v", facts)
	}
}

func TestRealPTYWritersResizeObserveAndHangup(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := startFakeJob(t, sandbox, "pty-clients", false, "synthetic integration task")

	_, writerA, writerADone, _ := startTerminal(t, job.cli, job.env, "enter", job.facts.Name)
	if !waitForClientModes(job.socket, 1, 0) {
		t.Fatal("first writable client did not attach")
	}
	if err := pty.Setsize(writerA, &pty.Winsize{Rows: 32, Cols: 96}); err != nil {
		t.Fatal(err)
	}
	waitForWindowSize(t, job, 96, 31)

	_, writerB, writerBDone, writerBOutput := startTerminal(t, job.cli, job.env, "enter", job.facts.Name)
	if !waitForClientModes(job.socket, 2, 0) {
		t.Fatal("second writable client did not attach")
	}
	if err := pty.Setsize(writerB, &pty.Winsize{Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	waitForWindowSize(t, job, 100, 39)

	_, observer, observerDone, observerOutput := startTerminal(t, job.cli, job.env, "observe", job.facts.Name)
	if !waitForClientModes(job.socket, 2, 1) {
		t.Fatal("read-only observer did not attach")
	}
	if err := pty.Setsize(observer, &pty.Winsize{Rows: 12, Cols: 40}); err != nil {
		t.Fatal(err)
	}
	waitForWindowSize(t, job, 100, 39)
	if _, err := observer.Write([]byte("OBSERVER_MUST_NOT_REACH_PI\n\x1b[200~PASTE_MUST_NOT_REACH_PI\x1b[201~\x1b[<0;1;1M\x1b[24~")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, observerDone)
	assertTerminalModesRestored(t, observerOutput)
	_ = observer.Close()

	if _, err := writerA.Write([]byte("WRITER_A_INPUT\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writerB.Write([]byte("WRITER_B_INPUT\n")); err != nil {
		t.Fatal(err)
	}
	waitForInput(t, job.inputPath, "WRITER_A_INPUT")
	waitForInput(t, job.inputPath, "WRITER_B_INPUT")
	input, err := os.ReadFile(job.inputPath)
	if err != nil || bytes.Contains(input, []byte("OBSERVER_MUST_NOT_REACH_PI")) || bytes.Contains(input, []byte("PASTE_MUST_NOT_REACH_PI")) {
		t.Fatalf("observer input reached Pi: %q, %v", input, err)
	}

	_ = writerA.Close()
	select {
	case <-writerADone:
		// A destroyed PTY has no remaining caller whose modes can be observed.
	case <-time.After(5 * time.Second):
		t.Fatal("hung-up writer client did not exit")
	}
	if !waitForClientModes(job.socket, 1, 0) {
		t.Fatal("remaining writer was removed after peer hangup")
	}
	if _, err := writerB.Write([]byte("WRITER_B_AFTER_PEER_HANGUP\n")); err != nil {
		t.Fatal(err)
	}
	waitForInput(t, job.inputPath, "WRITER_B_AFTER_PEER_HANGUP")
	if _, err := writerB.Write([]byte("\x1b[24~")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, writerBDone)
	assertTerminalModesRestored(t, writerBOutput)
	_ = writerB.Close()
	if facts := doctor(t, job.cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("writer hangup or F12 stopped the Pi session: %+v", facts)
	}
}

func TestRealPTYDashboardReturnErrorAndQuitRestoreTerminal(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := startFakeJob(t, sandbox, "dashboard-restore", false, "synthetic integration task")
	waitForCapture(t, job.capturePath)

	t.Run("return then human quit", func(t *testing.T) {
		_, terminal, done, output := startDashboard(t, job.cli, job.env)
		waitForOutput(t, output, job.facts.Name)
		if _, err := terminal.Write([]byte("\r")); err != nil {
			t.Fatal(err)
		}
		if !waitForClient(job.socket, "0") {
			t.Fatalf("dashboard Enter did not attach; output=%q", output.String())
		}
		if _, err := terminal.Write([]byte("\x1b[24~")); err != nil {
			t.Fatal(err)
		}
		waitForOutput(t, output, "Returned from attachment")
		if _, err := terminal.Write([]byte("q")); err != nil {
			t.Fatal(err)
		}
		waitForExit(t, done)
		assertTerminalModesRestored(t, output)
		if facts := doctor(t, job.cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
			t.Fatalf("dashboard Return/quit changed the Pi session: %+v", facts)
		}
	})

	t.Run("attach error then human quit", func(t *testing.T) {
		env := append(append([]string(nil), job.env...), "TMUX=/tmp/outer,1,0")
		_, terminal, done, output := startDashboard(t, job.cli, env)
		waitForOutput(t, output, job.facts.Name)
		if _, err := terminal.Write([]byte("\r")); err != nil {
			t.Fatal(err)
		}
		waitForOutput(t, output, "Enter and Observe require an ordinary terminal")
		if _, err := terminal.Write([]byte("q")); err != nil {
			t.Fatal(err)
		}
		waitForExit(t, done)
		assertTerminalModesRestored(t, output)
		if facts := doctor(t, job.cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
			t.Fatalf("dashboard error/quit changed the Pi session: %+v", facts)
		}
	})
}

func TestRealPTYNativePauseContinueRestoresUI(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	cli := sandbox.cli
	job := startFakeJob(t, sandbox, "continue-session", true, "synthetic integration task")
	capture := waitForCapture(t, job.capturePath)
	var launch struct {
		Pane struct {
			PID int `json:"pid"`
		} `json:"pane"`
	}
	content, err := os.ReadFile(filepath.Join(job.state, "jobs", job.facts.ID, "launch.json"))
	if err != nil || json.Unmarshal(content, &launch) != nil || launch.Pane.PID < 1 {
		t.Fatalf("read persisted pane identity: %v", err)
	}
	if capture.Env["TESTPI_NATIVE_PAUSE"] != "1" || capture.PID != launch.Pane.PID {
		t.Fatalf("native-pause fake identity/config mismatch: pid=%d pane=%d env=%v", capture.PID, launch.Pane.PID, capture.Env)
	}
	waitForMarker(t, job.inputReady)

	_, terminal, done, output := startTerminal(t, cli, job.env, "enter", job.facts.Name)
	if !waitForClient(job.socket, "0") {
		t.Fatal("writable terminal did not attach before Ctrl+Z")
	}
	if _, err := terminal.Write([]byte("\x1a")); err != nil {
		t.Fatal(err)
	}
	waitForMarker(t, job.pausedPath)
	if markerExists(job.restoredPath) {
		t.Fatal("fake UI restored before an explicit Continue")
	}
	if _, err := terminal.Write([]byte("\x1b[24~")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, done)
	assertTerminalModesRestored(t, output)
	_ = terminal.Close()

	_, terminal, done, output = startTerminal(t, cli, job.env, "continue", job.facts.Name)
	waitForOutput(t, output, "Type "+job.facts.Name+" to confirm")
	if _, err := terminal.Write([]byte(job.facts.Name + "\n")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, done)
	assertTerminalModesRestored(t, output)
	_ = terminal.Close()
	waitForMarker(t, job.restoredPath)

	_, terminal, done, output = startTerminal(t, cli, job.env, "enter", job.facts.Name)
	if !waitForClient(job.socket, "0") {
		t.Fatal("writable terminal did not reattach after Continue")
	}
	if _, err := terminal.Write([]byte("NATIVE_FOLLOWUP_ACCEPTED\n")); err != nil {
		t.Fatal(err)
	}
	waitForInput(t, job.inputPath, "NATIVE_FOLLOWUP_ACCEPTED")
	if _, err := terminal.Write([]byte("\x1b[24~")); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, done)
	assertTerminalModesRestored(t, output)
	_ = terminal.Close()
	if facts := doctor(t, cli, job); facts.Launch != "submitted" || facts.Terminal != "live" {
		t.Fatalf("native UI pause/Continue changed the job lifecycle: %+v", facts)
	}
}

type fakeJob struct {
	facts        launchFacts
	sandbox      *testSandbox
	root         string
	cwd          string
	cli          string
	env          []string
	state        string
	capturePath  string
	inputPath    string
	inputReady   string
	pausedPath   string
	restoredPath string
	release      string
	socket       string
}

type cliError struct {
	Error struct {
		ID string `json:"id"`
	} `json:"error"`
}

type processIdentity struct {
	BootID    string `json:"boot_id"`
	PID       int    `json:"pid"`
	UID       uint32 `json:"uid"`
	StartSec  int64  `json:"start_sec"`
	StartUsec int64  `json:"start_usec"`
}

type launchRecord struct {
	Phase  string           `json:"phase"`
	Server *processIdentity `json:"server,omitempty"`
	Pane   *processIdentity `json:"pane,omitempty"`
}

type diagnosticSnapshot struct {
	ID              string           `json:"id"`
	Manifest        json.RawMessage  `json:"manifest,omitempty"`
	Launch          json.RawMessage  `json:"launch,omitempty"`
	DoctorOutput    string           `json:"doctor_output,omitempty"`
	TmuxFacts       string           `json:"tmux_facts,omitempty"`
	TmuxError       string           `json:"tmux_error,omitempty"`
	KernelBootID    string           `json:"kernel_boot_id,omitempty"`
	KernelBootError string           `json:"kernel_boot_error,omitempty"`
	ServerCurrent   *processIdentity `json:"server_current,omitempty"`
	ServerError     string           `json:"server_error,omitempty"`
	ServerMatches   bool             `json:"server_matches_record,omitempty"`
	PaneCurrent     *processIdentity `json:"pane_current,omitempty"`
	PaneError       string           `json:"pane_error,omitempty"`
	PaneMatches     bool             `json:"pane_matches_record,omitempty"`
}

func collectStartDiagnostics(job *fakeJob) {
	var report diagnosticSnapshot
	report.ID = job.facts.ID
	manifestPath := filepath.Join(job.state, "jobs", job.facts.ID, "manifest.json")
	launchPath := filepath.Join(job.state, "jobs", job.facts.ID, "launch.json")
	manifestBytes, manifestErr := os.ReadFile(manifestPath)
	if manifestErr == nil {
		report.Manifest = json.RawMessage(manifestBytes)
	}
	launchBytes, launchErr := os.ReadFile(launchPath)
	var launch launchRecord
	if launchErr == nil {
		report.Launch = json.RawMessage(launchBytes)
		_ = json.Unmarshal(launchBytes, &launch)
	}
	var manifest struct {
		Socket string `json:"socket"`
		Tmux   string `json:"tmux"`
	}
	_ = json.Unmarshal(manifestBytes, &manifest)
	if manifest.Socket != "" {
		job.socket = manifest.Socket
	}
	doctor := exec.Command(job.cli, "doctor", job.facts.Name, "--json")
	doctor.Env = job.env
	if output, err := doctor.CombinedOutput(); err != nil {
		report.DoctorOutput = string(output)
		report.TmuxError = "doctor command returned an error: " + err.Error()
	} else {
		report.DoctorOutput = string(output)
	}
	if manifest.Tmux != "" && manifest.Socket != "" {
		format := strings.Join([]string{"#{@ac_owner}", "#{@ac_job}", "#{@ac_protocol}", "#{session_name}", "#{pane_id}", "#{pid}", "#{pane_pid}", "#{pane_dead}", "#{pane_dead_status}", "#{session_attached}", "#{window_width}", "#{window_height}"}, "\t")
		tmux := exec.Command(manifest.Tmux, "-S", manifest.Socket, "-N", "display-message", "-p", "-t", "=ac-"+job.facts.ID+":0.0", format)
		if output, err := tmux.CombinedOutput(); err != nil {
			report.TmuxError = err.Error()
		} else {
			report.TmuxFacts = string(output)
		}
	}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		report.KernelBootError = err.Error()
	} else {
		report.KernelBootID = strconv.FormatInt(boot.Sec, 10) + "." + strconv.FormatInt(int64(boot.Usec), 10)
	}
	if launch.Server != nil {
		report.ServerCurrent, report.ServerError = currentKernelIdentity(launch.Server.PID, report.KernelBootID)
		report.ServerMatches = sameProcessIdentity(launch.Server, report.ServerCurrent)
	}
	if launch.Pane != nil {
		report.PaneCurrent, report.PaneError = currentKernelIdentity(launch.Pane.PID, report.KernelBootID)
		report.PaneMatches = sameProcessIdentity(launch.Pane, report.PaneCurrent)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(job.sandbox.root, "diagnostics-"+job.facts.ID+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return
	}
	if file.Chmod(0600) == nil {
		_, _ = file.Write(append(encoded, '\n'))
	}
	_ = file.Close()
	job.sandbox.diagnosticPath = path
}

func currentKernelIdentity(pid int, bootID string) (*processIdentity, string) {
	if bootID == "" {
		return nil, "kernel boot identity unavailable"
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return nil, err.Error()
	}
	if len(processes) != 1 || int(processes[0].Proc.P_pid) != pid {
		return nil, "kernel process record unavailable or ambiguous"
	}
	process := &processes[0]
	return &processIdentity{BootID: bootID, PID: pid, UID: process.Eproc.Ucred.Uid, StartSec: process.Proc.P_starttime.Sec, StartUsec: int64(process.Proc.P_starttime.Usec)}, ""
}

func sameProcessIdentity(recorded, current *processIdentity) bool {
	return recorded != nil && current != nil && recorded.BootID == current.BootID && recorded.PID == current.PID && recorded.UID == current.UID && recorded.StartSec == current.StartSec && recorded.StartUsec == current.StartUsec
}

func startFakeJob(t *testing.T, sandbox *testSandbox, name string, nativePause bool, task string) *fakeJob {
	t.Helper()
	tmp, err := os.MkdirTemp(sandbox.root, "job-")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(tmp, 0700)
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	fakePath := filepath.Join(binDir, "pi")
	copyExecutable(t, sandbox.fake, fakePath)
	cwd := filepath.Join(tmp, "work space;#{format text}")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	capturePath, release := filepath.Join(tmp, "capture.json"), filepath.Join(tmp, "release")
	env := integrationEnv(sandbox.cli, fakePath, state, capturePath, release)
	if nativePause {
		env = append(env, "TESTPI_NATIVE_PAUSE=1")
	}
	job := &fakeJob{sandbox: sandbox, root: tmp, cwd: cwd, cli: sandbox.cli, env: env, state: state, capturePath: capturePath, inputPath: capturePath + ".input", inputReady: capturePath + ".input-ready", pausedPath: capturePath + ".paused", restoredPath: capturePath + ".restored", release: release}
	t.Cleanup(func() {
		if !cleanupFakeJob(t, job) {
			sandbox.preserve = true
		}
	})

	cmd := exec.Command(sandbox.cli, "start", name, "--cwd", cwd, "--task-file", "-", "--json")
	cmd.Stdin = strings.NewReader(task)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		var failure cliError
		if json.Unmarshal(output, &failure) == nil {
			job.facts = launchFacts{ID: failure.Error.ID, Name: name}
			if job.facts.ID != "" {
				collectStartDiagnostics(job)
				sandbox.diagnosticPath = filepath.Join(sandbox.root, "diagnostics-"+job.facts.ID+".json")
				sandbox.preserve = true
			}
		}
		t.Fatalf("start %s: %v: %s; diagnostics: %s", name, err, output, sandbox.diagnosticPath)
	}
	if err := json.Unmarshal(output, &job.facts); err != nil {
		t.Fatalf("decode launch facts: %v: %s", err, output)
	}
	job.socket = socketFromManifest(t, job)
	return job
}

func socketFromManifest(t *testing.T, job *fakeJob) string {
	t.Helper()
	var manifest struct {
		Socket string `json:"socket"`
	}
	content, err := os.ReadFile(filepath.Join(job.state, "jobs", job.facts.ID, "manifest.json"))
	if err != nil || json.Unmarshal(content, &manifest) != nil {
		t.Fatalf("read job manifest: %v", err)
	}
	return manifest.Socket
}

func cleanupFakeJob(t *testing.T, job *fakeJob) bool {
	t.Helper()
	if job.facts.ID == "" {
		entries, _ := os.ReadDir(filepath.Join(job.state, "jobs"))
		if len(entries) == 0 {
			_ = os.RemoveAll(job.root)
			return true
		}
		t.Logf("cannot identify uncertain job records; preserved %s", job.root)
		return false
	}
	jobDir := filepath.Join(job.state, "jobs", job.facts.ID)
	var manifest struct {
		Socket string `json:"socket"`
		Owner  string `json:"owner"`
	}
	manifestBytes, err := os.ReadFile(filepath.Join(jobDir, "manifest.json"))
	if err != nil || json.Unmarshal(manifestBytes, &manifest) != nil || manifest.Socket == "" {
		t.Logf("job manifest unavailable; preserved evidence at %s", job.root)
		return false
	}
	job.socket = manifest.Socket
	var launch launchRecord
	launchBytes, err := os.ReadFile(filepath.Join(jobDir, "launch.json"))
	if err != nil || json.Unmarshal(launchBytes, &launch) != nil || launch.Pane == nil || launch.Server == nil {
		t.Logf("process identities unavailable; preserved evidence at %s", job.root)
		return false
	}

	if err := os.WriteFile(job.release, nil, 0600); err != nil {
		t.Logf("fake Pi release failed; preserved %s: %v", job.root, err)
		return false
	}
	if ended, err := processIdentityEnded(*launch.Pane); err != nil {
		t.Logf("pane identity cannot be checked; preserved %s: %v", job.root, err)
		return false
	} else if !ended && markerExists(job.pausedPath) && !markerExists(job.restoredPath) {
		_, terminal, done, _ := startTerminal(t, job.cli, job.env, "continue", job.facts.Name)
		_, _ = terminal.Write([]byte(job.facts.Name + "\n"))
		select {
		case err := <-done:
			if err != nil {
				_ = terminal.Close()
				t.Logf("Continue cleanup failed; preserved %s: %v", job.root, err)
				return false
			}
		case <-time.After(5 * time.Second):
			_ = terminal.Close()
			t.Logf("Continue cleanup timed out; preserved %s", job.root)
			return false
		}
		_ = terminal.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	forgotten := false
	for time.Now().Before(deadline) {
		if !forgotten {
			paneEnded, paneErr := processIdentityEnded(*launch.Pane)
			if paneErr == nil && paneEnded {
				cmd := exec.Command(job.cli, "forget", "--id", job.facts.ID)
				cmd.Env = job.env
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Logf("Forget refused owned fixture %s: %v: %s", job.facts.ID, err, output)
				} else {
					forgotten = true
				}
			}
		}
		if forgotten && identitiesEnded(*launch.Server, *launch.Pane) && removeOwnedStaleSocket(job.socket, job.facts.ID, manifest.Owner, *launch.Server, *launch.Pane) {
			_ = os.RemoveAll(job.root)
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("job %s cleanup unconfirmed; preserved evidence at %s", job.facts.ID, job.root)
	return false
}

func identitiesEnded(server, pane processIdentity) bool {
	for _, identity := range []processIdentity{server, pane} {
		ended, err := processIdentityEnded(identity)
		if err != nil || !ended {
			return false
		}
	}
	return true
}

func processIdentityEnded(recorded processIdentity) (bool, error) {
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return false, err
	}
	bootID := strconv.FormatInt(boot.Sec, 10) + "." + strconv.FormatInt(int64(boot.Usec), 10)
	if recorded.BootID != bootID {
		return true, nil
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", recorded.PID)
	if err != nil {
		return false, err
	}
	if len(processes) == 0 {
		return true, nil
	}
	if len(processes) != 1 || int(processes[0].Proc.P_pid) != recorded.PID {
		return false, fmt.Errorf("kernel process identity is ambiguous for pid %d", recorded.PID)
	}
	process := &processes[0]
	current := processIdentity{BootID: bootID, PID: recorded.PID, UID: process.Eproc.Ucred.Uid, StartSec: process.Proc.P_starttime.Sec, StartUsec: int64(process.Proc.P_starttime.Usec)}
	return !sameProcessIdentity(&recorded, &current), nil
}

func removeOwnedStaleSocket(socket, id, owner string, server, pane processIdentity) bool {
	if !identitiesEnded(server, pane) {
		return false
	}
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return false
	}
	root := filepath.Join(tmp, fmt.Sprintf("ac-%d-%s", os.Getuid(), owner))
	dir := filepath.Join(root, id)
	if socket != filepath.Join(dir, "s") {
		return false
	}
	for _, path := range []string{root, dir} {
		info, err := os.Lstat(path)
		var stat unix.Stat_t
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || unix.Lstat(path, &stat) != nil || stat.Uid != uint32(os.Getuid()) {
			return false
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != "s") {
		return false
	}
	if len(entries) == 1 {
		var stat unix.Stat_t
		if err := unix.Lstat(socket, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Getuid()) {
			return false
		}
		if err := os.Remove(socket); err != nil {
			return false
		}
	}
	if err := os.Remove(dir); err != nil {
		return false
	}
	return true
}

func doctor(t *testing.T, cli string, job *fakeJob) launchFacts {
	t.Helper()
	facts, err := queryDoctor(cli, job.env, job.facts.Name)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func queryDoctor(cli string, env []string, name string) (launchFacts, error) {
	cmd := exec.Command(cli, "doctor", name, "--json")
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return launchFacts{}, err
	}
	var facts launchFacts
	err = json.Unmarshal(output, &facts)
	return facts, err
}

type terminalOutput struct {
	mu           sync.Mutex
	data         bytes.Buffer
	slave        *os.File
	savedTermios unix.Termios
}

func (o *terminalOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.data.Write(p)
}
func (o *terminalOutput) String() string { o.mu.Lock(); defer o.mu.Unlock(); return o.data.String() }

func startTerminal(t *testing.T, cli string, env []string, action, name string) (*exec.Cmd, *os.File, <-chan error, *terminalOutput) {
	t.Helper()
	cmd := exec.Command(cli, action, name)
	cmd.Env = env
	return startPTYCommand(t, cmd)
}

func startDashboard(t *testing.T, cli string, env []string) (*exec.Cmd, *os.File, <-chan error, *terminalOutput) {
	t.Helper()
	cmd := exec.Command(cli)
	cmd.Env = env
	return startPTYCommand(t, cmd)
}

// syntheticPTY responds to tmux's color/cursor queries, not physical keyboard behavior.
func startPTYCommand(t *testing.T, cmd *exec.Cmd) (*exec.Cmd, *os.File, <-chan error, *terminalOutput) {
	t.Helper()
	statusDir := t.TempDir()
	statusPath := filepath.Join(statusDir, "exit-status")
	releasePath := filepath.Join(statusDir, "release-terminal")
	// Keep the caller session alive: macOS revokes its TTY when the session leader exits.
	const supervisor = `umask 077
child=
interrupted=0
trap 'interrupted=1; if [ -n "$child" ]; then kill -TERM "$child"; fi' TERM
"$@" <&0 &
child=$!
while :; do
  interrupted=0
  wait "$child"
  status=$?
  [ "$interrupted" = 0 ] && break
done
child=
printf '%s\n' "$status" > "$TEST_TTY_STATUS"
while [ ! -f "$TEST_TTY_RELEASE" ]; do sleep 0.02; done
exit "$status"
`
	original := cmd
	cmd = exec.Command("/bin/sh", append([]string{"-c", supervisor, "terminal-caller"}, original.Args...)...)
	cmd.Env = append(append([]string(nil), original.Env...), "TEST_TTY_STATUS="+statusPath, "TEST_TTY_RELEASE="+releasePath)
	cmd.Dir = original.Dir
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		_ = terminal.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	saved, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	if err != nil {
		_ = terminal.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = terminal.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	output := &terminalOutput{slave: slave, savedTermios: *saved}
	go func() {
		buffer := make([]byte, 4096)
		colorAnswered, cursorAnswered := false, false
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				_, _ = output.Write(buffer[:n])
				observed := output.String()
				if !colorAnswered && strings.Contains(observed, "\x1b]11;?\x1b\\") {
					_, _ = terminal.Write([]byte("\x1b]11;rgb:ffff/ffff/ffff\x1b\\"))
					colorAnswered = true
				}
				if !cursorAnswered && strings.Contains(observed, "\x1b[6n") {
					_, _ = terminal.Write([]byte("\x1b[1;1R"))
					cursorAnswered = true
				}
			}
			if err != nil {
				return
			}
		}
	}()
	finished := make(chan struct{})
	var processError error
	go func() {
		processError = cmd.Wait()
		close(finished)
	}()
	done := make(chan error, 1)
	observe, stopObserving := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			status, err := os.ReadFile(statusPath)
			if err == nil && len(status) != 0 {
				code, err := strconv.Atoi(strings.TrimSpace(string(status)))
				if err == nil && code != 0 {
					err = fmt.Errorf("terminal command exited with status %d", code)
				}
				done <- err
				return
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				done <- err
				return
			}
			select {
			case <-finished:
				done <- processError
				return
			case <-observe.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	t.Cleanup(func() {
		stopObserving()
		if err := os.WriteFile(releasePath, nil, 0600); err != nil {
			t.Error(err)
		}
		select {
		case <-finished:
		default:
			_ = terminal.Close()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-finished:
				case <-time.After(2 * time.Second):
					t.Errorf("owned terminal caller %d did not exit during cleanup", cmd.Process.Pid)
				}
			}
		}
		_ = terminal.Close()
		_ = slave.Close()
	})
	return cmd, terminal, done, output
}

func assertTerminalModesRestored(t *testing.T, output *terminalOutput) {
	t.Helper()
	current, err := unix.IoctlGetTermios(int(output.slave.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatalf("read retained PTY slave termios after Return: %v", err)
	}
	if *current != output.savedTermios {
		t.Fatalf("PTY slave termios not restored: got=%+v want=%+v", *current, output.savedTermios)
	}
}

func waitForClient(socket, want string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		output, err := exec.CommandContext(ctx, "tmux", "-S", socket, "list-clients", "-F", "#{client_readonly}").Output()
		cancel()
		if err == nil && strings.TrimSpace(string(output)) == want {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func waitForClientModes(socket string, writable, readonly int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		output, err := exec.CommandContext(ctx, "tmux", "-S", socket, "list-clients", "-F", "#{client_readonly}").Output()
		cancel()
		if err == nil {
			w, r := 0, 0
			for _, mode := range strings.Fields(string(output)) {
				if mode == "0" {
					w++
				} else if mode == "1" {
					r++
				}
			}
			if w == writable && r == readonly {
				return true
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func waitForWindowSize(t *testing.T, job *fakeJob, width, height int) {
	t.Helper()
	want := fmt.Sprintf("%dx%d", width, height)
	deadline := time.Now().Add(5 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		cmd := exec.Command("tmux", "-S", job.socket, "display-message", "-p", "-t", "=ac-"+job.facts.ID+":0.0", "#{window_width}x#{window_height}")
		output, err := cmd.CombinedOutput()
		last, lastErr = strings.TrimSpace(string(output)), err
		if err == nil && last == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("tmux window size = %q (err %v), want %s", last, lastErr, want)
}

func waitForInput(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(content), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("writable terminal input %q did not reach fake Pi", want)
}

func waitForExit(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("terminal command failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal command did not detach")
	}
}

func waitForOutput(t *testing.T, output *terminalOutput, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("terminal did not prompt for confirmation: %q", output.String())
}

func markerExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func waitForMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if markerExists(path) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fixture state marker was not published: %s", path)
}

func waitForCapture(t *testing.T, path string) fakeCapture {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil {
			var capture fakeCapture
			if err := json.Unmarshal(content, &capture); err != nil {
				t.Fatalf("decode fake Pi capture: %v", err)
			}
			return capture
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read fake Pi capture: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake Pi did not write its private capture")
	return fakeCapture{}
}
