//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-coordinator/internal/coordinator"
)

func TestCommunicationCLIDiagnosticsAndCapabilityDoNotCreateCommunicationState(t *testing.T) {
	sandbox := buildBinaries(t)
	state := filepath.Join(t.TempDir(), "state")

	code, output := runCommunicationCLI(t, sandbox.cli, state, []string{"messages", "--json"}, nil)
	if code != 0 {
		t.Fatalf("messages --json exit %d: %s", code, output)
	}
	var diagnostics struct {
		Diagnostics struct {
			Messages  []json.RawMessage `json:"messages"`
			Pending   int               `json:"pending"`
			Escalated int               `json:"escalated"`
			Degraded  bool              `json:"degraded"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(output, &diagnostics); err != nil {
		t.Fatalf("decode empty diagnostics: %v: %s", err, output)
	}
	if diagnostics.Diagnostics.Messages == nil || len(diagnostics.Diagnostics.Messages) != 0 || diagnostics.Diagnostics.Pending != 0 || diagnostics.Diagnostics.Escalated != 0 || diagnostics.Diagnostics.Degraded {
		t.Fatalf("unexpected initial diagnostics: %+v", diagnostics.Diagnostics)
	}
	assertNoCommunicationEffects(t, state)

	code, output = runCommunicationCLI(t, sandbox.cli, state, []string{"_communication", "--json"}, []byte(`{"protocol":1,"action":"capability"}`))
	if code != 0 {
		t.Fatalf("capability exit %d: %s", code, output)
	}
	var capability struct {
		Capability struct {
			Protocol int `json:"protocol"`
		} `json:"capability"`
	}
	if err := json.Unmarshal(output, &capability); err != nil || capability.Capability.Protocol != coordinator.CommunicationProtocol {
		t.Fatalf("unexpected capability response: %v: %s", err, output)
	}
	assertNoCommunicationEffects(t, state)
}

func TestCommunicationCLIRejectsRequestsWithoutCommunicationEffects(t *testing.T) {
	sandbox := buildBinaries(t)
	validCapability := []byte(`{"protocol":1,"action":"capability"}`)
	oversizedCapability := append(append([]byte(nil), validCapability...), bytes.Repeat([]byte{' '}, coordinator.MaxCommunicationRequestBytes+1-len(validCapability))...)
	cases := []struct {
		name  string
		args  []string
		input []byte
	}{
		{name: "malformed JSON", args: []string{"_communication", "--json"}, input: []byte(`{"protocol":`)},
		{name: "unknown action", args: []string{"_communication", "--json"}, input: []byte(`{"protocol":1,"action":"future"}`)},
		{name: "irrelevant field", args: []string{"_communication", "--json"}, input: []byte(`{"protocol":1,"action":"capability","caller":{}}`)},
		{name: "unsupported protocol", args: []string{"_communication", "--json"}, input: []byte(`{"protocol":99,"action":"capability"}`)},
		{name: "oversized request", args: []string{"_communication", "--json"}, input: oversizedCapability},
		{name: "wrong parent identity", args: []string{"_communication", "--json"}, input: []byte(`{"protocol":1,"action":"enroll","caller":{"session_file":"/not/a/session","session_id":"not-native","generation":"123e4567-e89b-42d3-a456-426614174000","pid":1}}`)},
		{name: "negative diagnostics cursor", args: []string{"messages", "--json", "--cursor", "-1"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			code, output := runCommunicationCLI(t, sandbox.cli, state, test.args, test.input)
			if code == 0 {
				t.Fatalf("invalid request succeeded: %s", output)
			}
			var failure struct {
				Error struct {
					Code      string `json:"code"`
					Uncertain bool   `json:"uncertain"`
				} `json:"error"`
			}
			if err := json.Unmarshal(output, &failure); err != nil || failure.Error.Code == "" || failure.Error.Uncertain {
				t.Fatalf("rejection was not a definite structured failure: %v: %s", err, output)
			}
			assertNoCommunicationEffects(t, state)
		})
	}
}

func TestCommunicationCLIDiagnosticsRefuseUnknownSchemaWithoutRewrite(t *testing.T) {
	sandbox := buildBinaries(t)
	state := filepath.Join(t.TempDir(), "state")
	communicationDir := filepath.Join(state, "communication")
	if err := os.MkdirAll(communicationDir, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"schema":99,"protocol":1,"managers":[],"bindings":[],"messages":[],"seen":[]}`)
	path := filepath.Join(communicationDir, "state.json")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	code, output := runCommunicationCLI(t, sandbox.cli, state, []string{"messages", "--json"}, nil)
	if code == 0 {
		t.Fatalf("diagnostics accepted unknown schema: %s", output)
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(output, &failure); err != nil || failure.Error.Code != "operational_failure" {
		t.Fatalf("unknown schema did not reach the communication decoder: %v: %s", err, output)
	}
	persisted, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(persisted, original) {
		t.Fatalf("unknown schema was rewritten: %v", err)
	}
	assertNoJobProcesses(t, state)
}

func TestCommunicationCLIManagedStartPreservesTaskLimit(t *testing.T) {
	sandbox := buildBinaries(t)
	for _, character := range []string{"x", "\x00"} {
		t.Run(fmt.Sprintf("escaped-%t", character != "x"), func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			body, err := json.Marshal(coordinator.CommunicationRequest{
				Protocol: 1, Action: "start", Name: "worker", Cwd: t.TempDir(), Task: strings.Repeat(character, coordinator.MaxTaskBytes),
				Caller: &coordinator.CommunicationCaller{SessionFile: "/not/a/session", SessionID: "native", Generation: "123e4567-e89b-42d3-a456-426614174000", PID: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			code, output := runCommunicationCLI(t, sandbox.cli, state, []string{"_communication", "--json"}, body)
			var failure coordinator.ErrorResult
			if code != 1 || json.Unmarshal(output, &failure) != nil || failure.Error.Code != "caller_mismatch" {
				t.Fatalf("valid task must reach caller verification without dispatch: exit=%d %s", code, output)
			}
			assertNoCommunicationEffects(t, state)
		})
	}
}

func runCommunicationCLI(t *testing.T, cli, state string, args []string, input []byte) (int, []byte) {
	t.Helper()
	home := filepath.Join(filepath.Dir(state), "home")
	piDir := filepath.Join(filepath.Dir(state), "pi-config")
	for _, dir := range []string{home, piDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(cli, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "AC_STATE_DIR=" + state, "PI_CODING_AGENT_DIR=" + piDir}
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return 0, output
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), output
	}
	t.Fatalf("run coordinator CLI: %v", err)
	return -1, nil
}

func assertNoCommunicationEffects(t *testing.T, state string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(state, "communication")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request created communication state path: %v", err)
	}
	assertNoJobProcesses(t, state)
}

func assertNoJobProcesses(t *testing.T, state string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read isolated job records: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Fatalf("CLI rejection created job records/process targets: %v", names)
	}
}
