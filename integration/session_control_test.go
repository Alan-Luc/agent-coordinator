//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const fakeSessionID = "simulated-native-session"

// TestSessionControlFakePi is launched as the actual tmux pane process. Its
// endpoint simulates the extension protocol; it is not native Pi acceptance.
func TestSessionControlFakePi(t *testing.T) {
	jobID := os.Getenv("AC_JOB")
	if jobID == "" {
		return
	}
	state := os.Getenv("AC_STATE_DIR")
	manifestBytes, err := os.ReadFile(filepath.Join(state, "jobs", jobID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Socket string `json:"socket"`
		Owner  string `json:"owner"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Socket == "" || manifest.Owner == "" {
		t.Fatal("job manifest lacks endpoint identity")
	}
	controlDir := filepath.Dir(manifest.Socket)
	listener, err := net.Listen("unix", filepath.Join(controlDir, "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	statusPath := filepath.Join(state, "jobs", jobID, "session.json")
	publish := func() {
		status := map[string]any{
			"protocol": 1, "job_id": jobID, "owner": manifest.Owner,
			"session_id": fakeSessionID, "generation": jobID,
			"pid": os.Getpid(), "updated_at": time.Now().UnixMilli(), "activity": "idle",
		}
		body, _ := json.Marshal(status)
		if err := os.WriteFile(statusPath, append(body, '\n'), 0600); err != nil {
			t.Error(err)
		}
	}
	publish()
	finished := make(chan struct{}, 1)
	foregroundShift := make(chan chan error)
	go func() {
		for {
			_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(150 * time.Millisecond))
			conn, err := listener.Accept()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return
			}
			var request struct {
				Protocol   int    `json:"protocol"`
				JobID      string `json:"job_id"`
				SessionID  string `json:"session_id"`
				Generation string `json:"generation"`
				RequestID  string `json:"request_id"`
				Action     string `json:"action"`
				Text       string `json:"text,omitempty"`
			}
			decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request)
			if decodeErr == nil {
				if request.Text == "fixture-foreground-shift" && manifest.Name == "force-foreground-target" {
					ack := make(chan error)
					foregroundShift <- ack
					if err := <-ack; err != nil {
						_ = json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
						_ = conn.Close()
						continue
					}
				}
				line, _ := json.Marshal(request)
				f, fileErr := os.OpenFile(os.Getenv("TESTPI_INPUT_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if fileErr == nil {
					_, fileErr = f.Write(append(line, '\n'))
					_ = f.Close()
				}
				if fileErr != nil {
					t.Error(fileErr)
				}
				result := "api_invoked"
				if strings.Contains(manifest.Name, "invalid-receipt") {
					result = "work_succeeded"
				}
				receipt := map[string]any{"receipt": map[string]any{
					"id": jobID, "session_id": fakeSessionID, "generation": jobID,
					"request_id": request.RequestID, "operation": request.Action, "result": result,
				}}
				_ = json.NewEncoder(conn).Encode(receipt)
				if request.Action == "stop" && !strings.Contains(manifest.Name, "ignore-stop") {
					finished <- struct{}{}
				}
			}
			_ = conn.Close()
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	release := os.Getenv("TESTPI_RELEASE")
	var foregroundChild *exec.Cmd
	var foregroundChildDone chan struct{}
	defer func() {
		if foregroundChild != nil {
			if err := unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, os.Getpid()); err != nil {
				t.Error(err)
			}
			_ = foregroundChild.Process.Kill()
			<-foregroundChildDone
		}
	}()
	for {
		select {
		case ack := <-foregroundShift:
			if foregroundChild != nil {
				ack <- nil
				continue
			}
			child := exec.Command("/bin/sleep", "30")
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := child.Start(); err != nil {
				ack <- err
				continue
			}
			foregroundChild = child
			foregroundChildDone = make(chan struct{})
			go func() { _ = child.Wait(); close(foregroundChildDone) }()
			signal.Ignore(syscall.SIGTTOU)
			boot, err := currentKernelBootID()
			if err == nil {
				identity, detail := currentKernelIdentity(child.Process.Pid, boot)
				if detail != "" {
					err = fmt.Errorf("child kernel identity: %s", detail)
				} else {
					var body []byte
					body, err = json.Marshal(identity)
					if err == nil {
						err = os.WriteFile(filepath.Join(state, "jobs", jobID, "foreground.json"), body, 0600)
					}
				}
			}
			if err == nil {
				err = unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, child.Process.Pid)
			}
			if err == nil {
				processes, queryErr := unix.SysctlKinfoProcSlice("kern.proc.pid", os.Getpid())
				if queryErr != nil || len(processes) != 1 || int(processes[0].Eproc.Tpgid) != child.Process.Pid {
					err = fmt.Errorf("foreground handoff not observed: %v", queryErr)
				}
			}
			ack <- err
		case <-finished:
			return
		case <-ticker.C:
			publish()
		case <-time.After(100 * time.Millisecond):
			if _, err := os.Stat(release); err == nil {
				return
			}
		}
	}
}

func sessionControlSandbox(t *testing.T) *testSandbox {
	t.Helper()
	requireTmux(t)
	sandbox := buildBinaries(t)
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestSessionControlFakePi$'\n", os.Args[0])
	if err := os.WriteFile(sandbox.fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return sandbox
}

func sessionControlStatus(t *testing.T, job *fakeJob) (string, string) {
	t.Helper()
	path := filepath.Join(job.state, "jobs", job.facts.ID, "session.json")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			var status struct {
				SessionID  string `json:"session_id"`
				Generation string `json:"generation"`
			}
			if json.Unmarshal(body, &status) == nil && status.SessionID != "" && status.Generation != "" {
				return status.SessionID, status.Generation
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("simulated Pi did not publish session status")
	return "", ""
}

func TestSessionControlCLIJsonSessionInfoIsOptIn(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "session-info-wire", false, "wire fixture")
	sessionControlStatus(t, job)
	for _, command := range [][]string{{"list"}, {"doctor", job.facts.Name}} {
		for _, enabled := range []bool{false, true} {
			args := append(append([]string{}, command...), "--json")
			if enabled {
				args = append(args, "--session-info")
			}
			body := runCLIJSON(t, job.cli, job.env, args...)
			var value map[string]any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			contains := false
			if command[0] == "list" {
				jobs, _ := value["jobs"].([]any)
				if len(jobs) != 1 {
					t.Fatalf("list returned %d jobs", len(jobs))
				}
				_, contains = jobs[0].(map[string]any)["session"]
			} else {
				_, contains = value["session"]
			}
			if contains != enabled {
				t.Fatalf("%v session-info=%t emitted session=%t: %s", command, enabled, contains, body)
			}
		}
	}
}

func TestSessionControlSendTargetsOnlySelectedSession(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	target := startFakeJob(t, sandbox, "control-target", false, "target fixture")
	bystander := startFakeJob(t, sandbox, "control-bystander", false, "bystander fixture")
	sessionID, generation := sessionControlStatus(t, target)
	_, bystanderGeneration := sessionControlStatus(t, bystander)
	if generation == bystanderGeneration {
		t.Fatal("fixture generations must differ to detect accidental retargeting")
	}
	requestID := "123e4567-e89b-42d3-a456-426614174088"
	literal := "Approve — Build\n$(touch SHOULD_NOT_EXIST)\t世界"
	cmd := exec.Command(target.cli, "send", target.facts.ID, "--session-id", sessionID, "--generation", generation, "--request-id", requestID, "--json")
	cmd.Env = target.env
	cmd.Stdin = strings.NewReader(literal)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("send literal stdin: %v: %s", err, output)
	}
	var receipt struct {
		ID         string `json:"id"`
		SessionID  string `json:"session_id"`
		Generation string `json:"generation"`
		RequestID  string `json:"request_id"`
		Operation  string `json:"operation"`
		Result     string `json:"result"`
	}
	if err := json.Unmarshal(output, &receipt); err != nil {
		t.Fatalf("decode send receipt: %v: %s", err, output)
	}
	if receipt.ID != target.facts.ID || receipt.SessionID != sessionID || receipt.Generation != generation || receipt.RequestID != requestID || receipt.Operation != "send" || receipt.Result != "api_invoked" {
		t.Fatalf("send receipt does not bind the selected session: %+v", receipt)
	}
	var received []struct {
		JobID      string `json:"job_id"`
		SessionID  string `json:"session_id"`
		Generation string `json:"generation"`
		RequestID  string `json:"request_id"`
		Action     string `json:"action"`
		Text       string `json:"text"`
	}
	waitForCondition(t, "target endpoint delivery", func() bool {
		body, err := os.ReadFile(target.inputPath)
		return err == nil && json.Unmarshal([]byte("["+strings.TrimSpace(string(body))+"]"), &received) == nil && len(received) == 1
	})
	got := received[0]
	if got.JobID != target.facts.ID || got.SessionID != sessionID || got.Generation != generation || got.RequestID != requestID || got.Action != "send" || got.Text != literal {
		t.Fatalf("simulated Pi received altered or misdirected control: %+v", got)
	}
	if body, err := os.ReadFile(bystander.inputPath); err == nil && len(body) != 0 {
		t.Fatalf("bystander received a control request: %s", body)
	}
	stale := exec.Command(target.cli, "send", target.facts.ID, "--session-id", sessionID, "--generation", bystanderGeneration, "--request-id", "123e4567-e89b-42d3-a456-426614174087", "--json")
	stale.Env, stale.Stdin = target.env, strings.NewReader("must not dispatch")
	if output, err := stale.CombinedOutput(); err == nil {
		t.Fatalf("stale generation was accepted: %s", output)
	}
	body, err := os.ReadFile(target.inputPath)
	if err != nil || strings.Count(string(body), "\n") != 1 {
		t.Fatalf("stale generation caused an extra endpoint effect: %q, %v", body, err)
	}
}

func TestSessionControlExpiredStopAuthorizationCanBeReplaced(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "expired-control-auth", false, "authorization fixture")
	sessionID, generation := sessionControlStatus(t, job)
	prepareArgs := []string{"_prepare-stop", job.facts.ID, "--session-id", sessionID, "--generation", generation, "--json"}
	firstBody := runCLIJSON(t, job.cli, job.env, prepareArgs...)
	var first struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(firstBody, &first); err != nil || first.Token == "" || first.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("invalid initial stop authorization: %v: %s", err, firstBody)
	}
	authPath := filepath.Join(job.state, "jobs", job.facts.ID, "stop-authorization.json")
	var persisted map[string]any
	content, err := os.ReadFile(authPath)
	if err != nil || json.Unmarshal(content, &persisted) != nil {
		t.Fatalf("read owned authorization: %v", err)
	}
	persisted["expires_at"] = float64(1)
	content, err = json.Marshal(persisted)
	if err != nil || os.WriteFile(authPath, append(content, '\n'), 0600) != nil {
		t.Fatalf("expire exact owned authorization: %v", err)
	}
	secondBody := runCLIJSON(t, job.cli, job.env, prepareArgs...)
	var second struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(secondBody, &second); err != nil || second.Token == "" || second.Token == first.Token || second.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("fresh stop action did not receive a distinct live authorization: %v: %s", err, secondBody)
	}
	if body, err := os.ReadFile(job.inputPath); err == nil && len(body) != 0 {
		t.Fatalf("preparation dispatched a stop before human confirmation: %s", body)
	}
}

func TestSessionControlRejectsEndpointOwnedByWrongPeerBeforeDispatch(t *testing.T) {
	for _, action := range []string{"send", "stop"} {
		t.Run(action, func(t *testing.T) {
			sandbox := sessionControlSandbox(t)
			job := startFakeJob(t, sandbox, "wrong-peer-control", false, "peer fixture")
			session, generation := sessionControlStatus(t, job)
			manifestBody, err := os.ReadFile(filepath.Join(job.state, "jobs", job.facts.ID, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Socket string `json:"socket"`
			}
			if err := json.Unmarshal(manifestBody, &manifest); err != nil {
				t.Fatal(err)
			}
			endpoint := filepath.Join(filepath.Dir(manifest.Socket), "control")
			oldEndpoint := endpoint + ".old"
			if err := os.Rename(endpoint, oldEndpoint); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", endpoint)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(endpoint); _ = os.Rename(oldEndpoint, endpoint) })
			if action == "send" {
				cmd := exec.Command(job.cli, "send", job.facts.ID, "--session-id", session, "--generation", generation, "--request-id", "123e4567-e89b-42d3-a456-426614174089", "--json")
				cmd.Env, cmd.Stdin = job.env, strings.NewReader("must not dispatch")
				if output, err := cmd.CombinedOutput(); err == nil {
					t.Fatalf("wrong endpoint peer accepted send: %s", output)
				}
			} else {
				auth := runCLIJSON(t, job.cli, job.env, "_prepare-stop", job.facts.ID, "--session-id", session, "--generation", generation, "--json")
				cmd := exec.Command(job.cli, "_confirmed-stop", "--json")
				cmd.Env, cmd.Stdin = job.env, strings.NewReader(string(auth))
				if output, err := cmd.CombinedOutput(); err == nil {
					t.Fatalf("wrong endpoint peer accepted stop: %s", output)
				}
			}
			conn, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			var request [1]byte
			if n, _ := conn.Read(request[:]); n != 0 {
				t.Fatalf("wrong peer received request bytes: %q", request[:n])
			}
			if facts := doctor(t, job.cli, job); facts.Terminal != "live" {
				t.Fatalf("wrong-peer endpoint affected target: %+v", facts)
			}
		})
	}
}

func TestSessionControlFreshConfirmationSupersedesPendingGrant(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "pending-control-auth", false, "authorization fixture")
	session, generation := sessionControlStatus(t, job)
	args := []string{"_prepare-stop", job.facts.ID, "--session-id", session, "--generation", generation, "--json"}
	first := runCLIJSON(t, job.cli, job.env, args...)
	second := runCLIJSON(t, job.cli, job.env, args...)
	var firstAuth, secondAuth map[string]any
	if json.Unmarshal(first, &firstAuth) != nil || json.Unmarshal(second, &secondAuth) != nil || firstAuth["token"] == secondAuth["token"] {
		t.Fatalf("fresh confirmed action did not rotate pending grant: first=%s second=%s", first, second)
	}
	cmd := exec.Command(job.cli, "_confirmed-stop", "--json")
	cmd.Env, cmd.Stdin = job.env, strings.NewReader(string(first))
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("superseded authorization was accepted: %s", output)
	}
	if body, err := os.ReadFile(job.inputPath); err == nil && len(body) != 0 {
		t.Fatalf("superseded authorization dispatched: %s", body)
	}
}

func TestSessionControlCorruptAuthorizationDoesNotWedgeFreshAction(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "corrupt-control-auth", false, "authorization fixture")
	session, generation := sessionControlStatus(t, job)
	args := []string{"_prepare-stop", job.facts.ID, "--session-id", session, "--generation", generation, "--json"}
	_ = runCLIJSON(t, job.cli, job.env, args...)
	authPath := filepath.Join(job.state, "jobs", job.facts.ID, "stop-authorization.json")
	if err := os.WriteFile(authPath, []byte(`{"token":`), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := runCLIJSON(t, job.cli, job.env, args...)
	var auth struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(fresh, &auth); err != nil || auth.Token == "" {
		t.Fatalf("fresh action remained wedged by interrupted authorization: %v: %s", err, fresh)
	}
}

func TestSessionControlRejectsChangedAuthorizationOperation(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "changed-control-auth", false, "authorization fixture")
	session, generation := sessionControlStatus(t, job)
	body := runCLIJSON(t, job.cli, job.env, "_prepare-stop", job.facts.ID, "--session-id", session, "--generation", generation, "--json")
	var auth map[string]any
	if err := json.Unmarshal(body, &auth); err != nil {
		t.Fatal(err)
	}
	auth["operation"] = "force_stop"
	changed, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(job.cli, "_confirmed-stop", "--json")
	cmd.Env, cmd.Stdin = job.env, strings.NewReader(string(changed))
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("operation-tampered authorization accepted: %s", output)
	}
	if body, err := os.ReadFile(job.inputPath); err == nil && len(body) != 0 {
		t.Fatalf("operation-tampered authorization dispatched: %s", body)
	}
}

func TestSessionControlRejectsInvalidReceiptResult(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "invalid-receipt", false, "receipt fixture")
	session, generation := sessionControlStatus(t, job)
	cmd := exec.Command(job.cli, "send", job.facts.ID, "--session-id", session, "--generation", generation, "--request-id", "123e4567-e89b-42d3-a456-426614174088", "--json")
	cmd.Env, cmd.Stdin = job.env, strings.NewReader("literal instruction")
	output, err := cmd.CombinedOutput()
	if code := commandExitCode(err); code != 75 {
		t.Fatalf("invalid post-dispatch receipt must be uncertain (75), got %d: %s", code, output)
	}
}

func TestSessionControlConfirmedStopsAffectOnlyVerifiedTarget(t *testing.T) {
	for _, operation := range []string{"stop", "force_stop", "ignore-stop"} {
		t.Run(operation, func(t *testing.T) {
			sandbox := sessionControlSandbox(t)
			job := startFakeJob(t, sandbox, "control-"+strings.ReplaceAll(operation, "_", "-"), false, "owned stop fixture")
			bystander := startFakeJob(t, sandbox, "control-bystander", false, "bystander fixture")
			session, generation := sessionControlStatus(t, job)
			sessionControlStatus(t, bystander)
			prepare := []string{"_prepare-stop", job.facts.ID, "--session-id", session, "--generation", generation, "--json"}
			if operation == "force_stop" {
				prepare = append(prepare, "--force")
			}
			authorization := runCLIJSON(t, job.cli, job.env, prepare...)
			confirm := func() ([]byte, error) {
				cmd := exec.Command(job.cli, "_confirmed-stop", "--json")
				cmd.Env, cmd.Stdin = job.env, strings.NewReader(string(authorization))
				return cmd.CombinedOutput()
			}
			output, err := confirm()
			if operation == "ignore-stop" {
				if commandExitCode(err) != 75 {
					t.Fatalf("graceful timeout must remain uncertain, not escalate: %v: %s", err, output)
				}
				if facts := doctor(t, job.cli, job); facts.Terminal != "live" {
					t.Fatalf("timed-out graceful request stopped Pi: %+v", facts)
				}
			} else {
				if err != nil {
					t.Fatalf("confirmed fake-Pi stop: %v: %s", err, output)
				}
				var receipt struct {
					Result    string `json:"result"`
					Operation string `json:"operation"`
				}
				if json.Unmarshal(output, &receipt) != nil || receipt.Operation != operation {
					t.Fatalf("wrong stop receipt: %s", output)
				}
				if operation == "stop" && receipt.Result != "exit_observed" {
					t.Fatalf("graceful stop must observe exit: %s", output)
				}
				waitForCondition(t, "positive target exit", func() bool { return doctor(t, job.cli, job).Launch == "exited" })
			}
			if facts := doctor(t, bystander.cli, bystander); facts.Terminal != "live" {
				t.Fatalf("stop affected bystander: %+v", facts)
			}
			if output, err := confirm(); err == nil {
				t.Fatalf("single-use authorization replay accepted: %s", output)
			}
		})
	}
}

func TestSessionControlAuthorizationExpiresWhileWaitingForLock(t *testing.T) {
	sandbox := sessionControlSandbox(t)
	job := startFakeJob(t, sandbox, "expiring-control-auth", false, "lock expiry fixture")
	session, generation := sessionControlStatus(t, job)
	authorization := runCLIJSON(t, job.cli, job.env, "_prepare-stop", job.facts.ID, "--session-id", session, "--generation", generation, "--json")
	var auth map[string]any
	if err := json.Unmarshal(authorization, &auth); err != nil {
		t.Fatal(err)
	}
	auth["expires_at"] = time.Now().Add(time.Second).UnixMilli()
	authorization, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(job.state, "jobs", job.facts.ID)
	if err := os.WriteFile(filepath.Join(dir, "stop-authorization.json"), authorization, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "launch.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	cmd := exec.Command(job.cli, "_confirmed-stop", "--json")
	cmd.Env, cmd.Stdin = job.env, strings.NewReader(string(authorization))
	done := make(chan struct {
		output []byte
		err    error
	}, 1)
	go func() {
		output, err := cmd.CombinedOutput()
		done <- struct {
			output []byte
			err    error
		}{output, err}
	}()
	time.Sleep(1200 * time.Millisecond)
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if commandExitCode(result.err) != 2 {
		t.Fatalf("expired authorization dispatched after lock wait: %v: %s", result.err, result.output)
	}
	if body, err := os.ReadFile(job.inputPath); err == nil && len(body) != 0 {
		t.Fatalf("expired authorization dispatched: %s", body)
	}
	if facts := doctor(t, job.cli, job); facts.Terminal != "live" {
		t.Fatalf("expired confirmation stopped target: %+v", facts)
	}
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
