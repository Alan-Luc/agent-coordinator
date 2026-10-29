package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/creack/pty"
)

func TestKittyEligibilityUsesTTYIdentityNotGeometry(t *testing.T) {
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer terminal.Close()
	if err := pty.Setsize(terminal, &pty.Winsize{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "xterm-kitty")
	t.Setenv("KITTY_WINDOW_ID", "fixture")
	t.Setenv("TMUX", "")
	if !kittyEligible(terminal, terminal) {
		t.Fatal("a Kitty TTY with unavailable geometry must not silently bypass persistence")
	}
	var redirected bytes.Buffer
	if kittyEligible(terminal, &redirected) {
		t.Fatal("redirected output wrapped")
	}
	t.Setenv("TMUX", "/test/socket,1,0")
	if kittyEligible(terminal, terminal) {
		t.Fatal("nested tmux wrapped")
	}
}

func TestLaunchFeaturesDoNotOpenState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("AC_STATE_DIR", state)
	var output, diagnostic bytes.Buffer
	if code := run([]string{"_features", "--json"}, os.Stdin, &output, &diagnostic); code != 0 {
		t.Fatalf("features exit %d: %s", code, diagnostic.String())
	}
	var result struct {
		Features struct {
			InteractiveLaunch int    `json:"interactive_launch"`
			TerminalProfile   string `json:"terminal_profile"`
			ManagerHost       int    `json:"manager_host"`
			PendingManager    int    `json:"pending_manager"`
		} `json:"features"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Features.InteractiveLaunch != 1 || result.Features.TerminalProfile != "transparent-v1" || result.Features.ManagerHost != 1 || result.Features.PendingManager != 1 {
		t.Fatalf("unexpected features: %s", output.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("feature probe touched state: %v", err)
	}
}

func TestPiNativePassthroughPreservesProcessContract(t *testing.T) {
	if os.Getenv("TEST_PI_PASSTHROUGH_CHILD") == "1" {
		os.Exit(run([]string{"pi", "--json", "", "literal $(touch malicious)", "@image.png"}, os.Stdin, os.Stdout, os.Stderr))
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// The synthetic native executable reports exact argv without any Pi startup.
	fixture := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$TEST_NATIVE_CONFIG\"\nprintf '<%s>\\n' \"$@\"\nexit 23\n"
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPiNativePassthroughPreservesProcessContract$")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "TEST_PI_PASSTHROUGH_CHILD=1", "PATH="+bin, "AC_STATE_DIR="+state, "TEST_NATIVE_CONFIG=retained", "TERM=xterm-kitty", "KITTY_WINDOW_ID=fixture")
	output, err := cmd.CombinedOutput()
	var code int
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	}
	if code != 23 {
		t.Fatalf("native exit %d, want23: %s (%v)", code, output, err)
	}
	actual := bytes.Split(bytes.TrimSuffix(output, []byte("\n")), []byte("\n"))
	expected := [][]byte{[]byte(root), []byte("retained"), []byte("<--json>"), []byte("<>"), []byte("<literal $(touch malicious)>"), []byte("<@image.png>")}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("native process contract changed: %q", output)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("passthrough created coordinator state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "malicious")); !os.IsNotExist(err) {
		t.Fatalf("argv was shell evaluated: %v", err)
	}
}
