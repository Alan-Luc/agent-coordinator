package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-coordinator/internal/coordinator"
)

func TestRejectInvalidRequestsWithoutLeakingTask(t *testing.T) {
	const privateTask = "PRIVATE_TEST_TASK\n$(touch SHOULD_NOT_EXIST) </file>"
	cases := []struct {
		name string
		args []string
		task string
	}{
		{name: "missing cwd", args: []string{"start", "example", "--task-file", "-", "--json"}, task: privateTask},
		{name: "options before name", args: []string{"start", "--cwd", "/tmp", "example", "--task-file", "-", "--json"}, task: privateTask},
		{name: "unknown flag", args: []string{"list", "--force", "--json"}},
		{name: "invalid UTF-8", args: []string{"start", "example", "--cwd", "/tmp", "--task-file", "-", "--json"}, task: string([]byte{0xff})},
		{name: "blank task", args: []string{"start", "example", "--cwd", "/tmp", "--task-file", "-", "--json"}, task: " \n\t"},
		{name: "oversize task", args: []string{"start", "example", "--cwd", "/tmp", "--task-file", "-", "--json"}, task: strings.Repeat("x", coordinator.MaxTaskBytes+1)},
		{name: "invalid name", args: []string{"start", strings.Repeat("INVALID", 1000), "--cwd", "/tmp", "--task-file", "-", "--json"}, task: privateTask},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AC_STATE_DIR", filepath.Join(t.TempDir(), "state"))
			input := inputFile(t, test.task)
			var output, errors bytes.Buffer
			if code := run(test.args, input, &output, &errors); code != 2 {
				t.Fatalf("exit code %d, expected invalid usage", code)
			}
			if strings.Contains(output.String()+errors.String(), privateTask) {
				t.Fatal("private task leaked into diagnostics")
			}
			if output.Len() > 1024 {
				t.Fatal("invalid request produced unbounded diagnostics")
			}
			var result coordinator.ErrorResult
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatalf("diagnostic is not JSON: %v", err)
			}
			if result.Error.Code == "" || result.Error.Uncertain {
				t.Fatal("invalid request was not a definite structured failure")
			}
		})
	}
}

func TestListAcceptsSessionInfoFlag(t *testing.T) {
	t.Setenv("AC_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, args := range [][]string{{"list", "--json"}, {"list", "--session-info", "--json"}} {
		input := inputFile(t, "")
		var output, errors bytes.Buffer
		if code := run(args, input, &output, &errors); code != 0 {
			t.Fatalf("list args %v: exit %d: %s", args, code, errors.String())
		}
		var result coordinator.ListResult
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatalf("list args %v returned invalid JSON: %v", args, err)
		}
	}
}

func TestDashboardRejectsNonTerminalWithUsageExit(t *testing.T) {
	t.Setenv("AC_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	input := inputFile(t, "")
	originalInput := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = originalInput })
	var output, errors bytes.Buffer
	if code := run(nil, input, &output, &errors); code != 2 {
		t.Fatalf("exit code %d, expected 2 for unsupported terminal", code)
	}
}

func TestStopRequiresInteractiveConfirmationBeforeTargetLookup(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("AC_STATE_DIR", state)
	input := inputFile(t, "")
	var output, errors bytes.Buffer
	const id = "123e4567-e89b-42d3-a456-426614174000"
	if code := run([]string{"stop", id}, input, &output, &errors); code != 2 {
		t.Fatalf("non-terminal stop exit = %d, want 2", code)
	}
	entries, err := os.ReadDir(filepath.Join(state, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stop created job-side effects before confirmation: %v", entries)
	}
}

func inputFile(t *testing.T, body string) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return file
}
