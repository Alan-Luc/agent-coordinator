package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"coordinate/internal/pilaunch"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "_run" {
		if err := RunBootstrap(context.Background(), os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStartInteractiveUsesOwnedTmuxTransactionAndPreservesNativeArgv(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable for managed-layout fixture")
	}
	root := t.TempDir()
	capturePath := filepath.Join(root, "argv.json")
	t.Setenv("ARGV_CAPTURE", capturePath)
	managed := interactiveManagedFixture(t)
	store, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	coordinatorPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{store: store, options: Options{TmuxPath: tmuxPath, Executable: coordinatorPath, Width: 80, Height: 24}}
	var jobID string
	t.Cleanup(func() {
		if idPattern.MatchString(jobID) {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = coordinator.Forget(cleanupCtx, jobID, true)
		}
	})
	wantArgv := []string{"", "@image.png", "$(not a shell command)", "--model", "literal value", "é"}
	facts, err := coordinator.StartInteractive(context.Background(), root, wantArgv, managed)
	if err != nil {
		var action *ActionError
		if errors.As(err, &action) {
			jobID = action.Diagnostic.ID
		}
		t.Fatalf("StartInteractive: %v", err)
	}
	jobID = facts.ID
	if facts.ID == "" || facts.Name != "pi-"+facts.ID[:8] {
		t.Fatalf("interactive identity = %+v", facts)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = coordinator.Forget(cleanupCtx, facts.ID, true)
	})
	job := filepath.Join(store.root, "jobs", facts.ID)
	if _, err := os.Stat(filepath.Join(job, "launch-argv.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap did not consume argv payload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(job, "initial-task.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interactive launch created a task envelope: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, doctorErr := coordinator.DoctorID(context.Background(), facts.ID)
		if doctorErr == nil && current.Launch == "exited" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	encodedArgv, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("fixture did not receive native argv: %v", err)
	}
	var gotArgv []string
	if err := json.Unmarshal(encodedArgv, &gotArgv); err != nil || !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Fatalf("native argv = %#v, decode error %v; want %#v", gotArgv, err, wantArgv)
	}
	result, err := coordinator.FinishInteractiveAttach(context.Background(), facts.ID, nil)
	if err != nil {
		t.Fatalf("FinishInteractiveAttach: %v", err)
	}
	if result.Detached || result.Uncertain || result.ExitStatus == nil || *result.ExitStatus != 7 || !strings.Contains(result.Transcript, "fixture Pi exited") {
		t.Fatalf("interactive exit result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(job, "interactive-outcome.json")); err != nil {
		t.Fatalf("verified outcome was not persisted: %v", err)
	}
}

func interactiveManagedFixture(t *testing.T) pilaunch.Managed {
	t.Helper()
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	launcher := filepath.Join(agent, "bin", "pi")
	packageDir := filepath.Join(agent, "install", "releases", "1.4.0", "node_modules", "@earendil-works", "pi-coding-agent")
	entry := filepath.Join(packageDir, "dist", "bundle", "cli.js")
	binDir := filepath.Join(agent, "install", "releases", "1.4.0", "node_modules", ".bin")
	for _, dir := range []string{filepath.Dir(launcher), filepath.Dir(entry), binDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	launcherBytes, err := os.ReadFile(filepath.Join("..", "pilaunch", "testdata", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(launcherBytes)
	if hex.EncodeToString(sum[:]) != "da0c6a49fe94aadc9765cc7afc6ba6b17e553eed0247d0385c160bc188f5cfbb" {
		t.Fatal("managed launcher fixture hash changed")
	}
	if err := os.WriteFile(launcher, launcherBytes, 0700); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(agent, "install")
	if err := os.WriteFile(filepath.Join(installDir, "current-version"), []byte("1.4.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","version":"1.4.0","bin":{"pi":"dist/bundle/cli.js"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := "#!/usr/bin/env node\nrequire('node:fs').writeFileSync(process.env.ARGV_CAPTURE, JSON.stringify(process.argv.slice(2))); console.log('fixture Pi exited'); setTimeout(() => process.exit(7), 800);\n"
	if err := os.WriteFile(entry, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../@earendil-works/pi-coding-agent/dist/bundle/cli.js", filepath.Join(binDir, "pi")); err != nil {
		t.Fatal(err)
	}
	managed, err := pilaunch.ObserveManaged(launcher)
	if err != nil {
		t.Fatal(err)
	}
	return managed
}

func TestInteractiveLaunchEnvironmentStripsWindowLineageAndKeepsPiDefaults(t *testing.T) {
	got := map[string]string{}
	for _, entry := range interactiveLaunchEnvironmentFrom([]string{
		"AC_INTRUDER=parent",
		"PI_SESSION_ID=old-session",
		"PI_SESSION_FILE=/old/session.jsonl",
		"KITTY_WINDOW_ID=42",
		"KITTY_LISTEN_ON=unix:/tmp/kitty",
		"KITTY_PID=123",
		"WINDOWID=9",
		"PI_PROVIDER=anthropic",
		"PI_MODEL=opus",
		"PI_REASONING_LEVEL=high",
		"PI_CODING_AGENT_DIR=/private/config",
	}, "/state", "owner", "job") {
		key, value, _ := strings.Cut(entry, "=")
		got[key] = value
	}
	for _, key := range []string{"AC_INTRUDER", "PI_SESSION_ID", "PI_SESSION_FILE", "KITTY_WINDOW_ID", "KITTY_LISTEN_ON", "KITTY_PID", "WINDOWID"} {
		if _, ok := got[key]; ok {
			t.Errorf("inherited lineage leaked: %s", key)
		}
	}
	for key, want := range map[string]string{"AC_STATE_DIR": "/state", "AC_OWNER": "owner", "AC_JOB": "job", "PI_PROVIDER": "anthropic", "PI_MODEL": "opus", "PI_REASONING_LEVEL": "high", "PI_CODING_AGENT_DIR": "/private/config"} {
		if got[key] != want {
			t.Errorf("%s = %q; want %q", key, got[key], want)
		}
	}
}

func TestStagedLaunchLockSurvivesDirectoryPublication(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(store.root, "jobs", ".staging-test")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := privateFile(filepath.Join(staging, "launch.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockStagedLaunch(file); err != nil {
		t.Fatal(err)
	}
	job := filepath.Join(store.root, "jobs", "123e4567-e89b-42d3-a456-426614174078")
	if err := os.Rename(staging, job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := store.lock(ctx, filepath.Join("jobs", filepath.Base(job), "launch.lock")); err == nil {
		t.Fatal("published launch lock inode was not held")
	}
	unlockStagedLaunch(file)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.lock(context.Background(), filepath.Join("jobs", filepath.Base(job), "launch.lock"))
	if err != nil {
		t.Fatalf("published launch lock remained held: %v", err)
	}
	unlock()
}

func TestInteractiveControlRequiresFreshLaunchProtocolMarker(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol string
		marker   string
		want     bool
	}{
		{name: "legacy remains compatible", protocol: ProtocolVersion, want: true},
		{name: "interactive missing marker refused", protocol: InteractiveProtocolVersion, want: false},
		{name: "interactive exact marker accepted", protocol: InteractiveProtocolVersion, marker: InteractiveProtocolVersion, want: true},
		{name: "interactive unknown marker refused", protocol: InteractiveProtocolVersion, marker: "3", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionLaunchProtocolMatches(SessionStatus{LaunchProtocol: test.marker}, test.protocol); got != test.want {
				t.Fatalf("sessionLaunchProtocolMatches() = %v; want %v", got, test.want)
			}
		})
	}
}

func TestSanitizeInteractiveOutputBoundsAndRemovesControls(t *testing.T) {
	got := sanitizeInteractiveOutput("ok\x1b[31mred\x00\nmore", 8)
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\x00') || len(got) > 8 || !strings.Contains(got, "ok") {
		t.Fatalf("unexpected sanitized output %q", got)
	}
}

func TestStartInteractiveRejectsUnobservedNativeBeforePublication(t *testing.T) {
	coordinator := &Coordinator{}
	_, err := coordinator.StartInteractive(context.Background(), ".", []string{"prompt"}, pilaunch.Managed{})
	var action *ActionError
	if !errors.As(err, &action) || action.Diagnostic.Code != "invalid_native_pi" {
		t.Fatalf("StartInteractive error = %v; want invalid_native_pi before publication", err)
	}
}
