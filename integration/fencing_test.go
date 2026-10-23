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
	"time"
)

func TestRealCLIForgetFencesDelayedSubmissionAndAllowsNameReuse(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	job := newSharedFakeJob(t, sandbox, "fenced-attempt")
	registerSharedCleanup(t, job)
	proxyDir := filepath.Join(job.root, "proxy-bin")
	if err := os.Mkdir(proxyDir, 0700); err != nil {
		t.Fatal(err)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	ready, release := filepath.Join(job.root, "tmux-ready"), filepath.Join(job.root, "tmux-release")
	proxy := filepath.Join(proxyDir, "tmux")
	script := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = new-session ]; then\n    : > \"$TEST_TMUX_GATE_READY\"\n    while [ ! -f \"$TEST_TMUX_GATE_RELEASE\" ]; do sleep 0.02; done\n    break\n  fi\ndone\nexec \"$TEST_REAL_TMUX\" \"$@\"\n"
	if err := os.WriteFile(proxy, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	job.env = replaceEnv(job.env, "PATH", proxyDir+string(os.PathListSeparator)+envValue(job.env, "PATH"))
	job.env = append(job.env, "TEST_REAL_TMUX="+tmuxPath, "TEST_TMUX_GATE_READY="+ready, "TEST_TMUX_GATE_RELEASE="+release)
	start := exec.Command(job.cli, "start", job.facts.Name, "--cwd", job.cwd, "--task-file", "-", "--json")
	start.Env = job.env
	start.Stdin = strings.NewReader("fencing synthetic task")
	startOutput := make(chan struct {
		bytes []byte
		err   error
	}, 1)
	go func() {
		out, err := start.CombinedOutput()
		startOutput <- struct {
			bytes []byte
			err   error
		}{out, err}
	}()
	waitForPath(t, ready)
	entries, err := os.ReadDir(filepath.Join(job.state, "jobs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("identify gated attempt from private registry: entries=%v err=%v", entries, err)
	}
	job.facts.ID = entries[0].Name()
	job.socket = socketFromManifest(t, job)

	forget := exec.Command(job.cli, "forget", job.facts.Name)
	forget.Env = job.env
	if output, err := forget.CombinedOutput(); err != nil {
		t.Fatalf("Forget unclaimed gated attempt: %v: %s", err, output)
	}
	list := runCLIJSON(t, job.cli, job.env, "list", "--json")
	if !strings.Contains(string(list), `"launch":"revoked"`) || !strings.Contains(string(list), `"id":"`+job.facts.ID+`"`) {
		t.Fatalf("revoked UUID tombstone missing from list: %s", list)
	}

	// A retired name can be reused independently while the delayed old UUID remains fenced.
	newJob := newSharedFakeJob(t, sandbox, job.facts.Name)
	registerSharedCleanup(t, newJob)
	newJob.facts = startSharedFakeJob(t, newJob, "replacement task")
	if newJob.facts.ID == job.facts.ID {
		t.Fatal("replacement reused the fenced UUID")
	}
	if got := waitForCapture(t, newJob.capturePath); got.Body != taskHeader+"replacement task" {
		t.Fatalf("replacement fake Pi body = %q", got.Body)
	}
	_ = os.WriteFile(release, nil, 0600)
	select {
	case result := <-startOutput:
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if result.err == nil || json.Unmarshal(result.bytes, &failure) != nil || failure.Error.Code != "launch_failed" {
			t.Fatalf("revoked delayed start did not report fenced launch failure: err=%v output=%s", result.err, result.bytes)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("delayed start did not return after releasing tmux submission")
	}
	if _, err := os.Stat(job.capturePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fenced delayed launch executed fake Pi: stat error %v", err)
	}
	var oldFacts struct {
		Jobs []launchFacts `json:"jobs"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listed := runCLIJSON(t, job.cli, job.env, "list", "--json")
		if err := json.Unmarshal(listed, &oldFacts); err != nil {
			t.Fatalf("decode revoked residue list: %v", err)
		}
		found := false
		for _, facts := range oldFacts.Jobs {
			if facts.ID == job.facts.ID && facts.Launch == "revoked" && facts.Terminal == "exited" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	retiredDeadPane := false
	for _, facts := range oldFacts.Jobs {
		if facts.ID == job.facts.ID && facts.Launch == "revoked" && facts.Terminal == "exited" {
			retiredDeadPane = true
		}
	}
	if !retiredDeadPane {
		t.Fatalf("late revoked pane did not become an explicit cleanup-needed residue: %+v", oldFacts.Jobs)
	}
	old := exec.Command(job.cli, "forget", "--id", job.facts.ID)
	old.Env = job.env
	if output, err := old.CombinedOutput(); err != nil {
		t.Fatalf("clean exact retired UUID: %v: %s", err, output)
	}
	if got := doctor(t, job.cli, newJob); got.ID != newJob.facts.ID || got.Launch != "submitted" {
		t.Fatalf("old UUID cleanup affected replacement name: %+v", got)
	}
}

// newSharedFakeJob creates a Pi identity rooted in an installation-wide registry.
func newSharedFakeJob(t *testing.T, sandbox *testSandbox, name string) *fakeJob {
	t.Helper()
	root, err := os.MkdirTemp(sandbox.root, "shared-job-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "pi")
	copyExecutable(t, sandbox.fake, fake)
	cwd := filepath.Join(root, "cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(sandbox.root, "shared-state")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	capture, release := filepath.Join(root, "capture.json"), filepath.Join(root, "release")
	return &fakeJob{
		facts: launchFacts{Name: name}, sandbox: sandbox, root: root, cwd: cwd, cli: sandbox.cli,
		env: integrationEnv(sandbox.cli, fake, state, capture, release), state: state,
		capturePath: capture, inputPath: capture + ".input", release: release,
	}
}

func startSharedFakeJob(t *testing.T, job *fakeJob, task string) launchFacts {
	t.Helper()
	cmd := exec.Command(job.cli, "start", job.facts.Name, "--cwd", job.cwd, "--task-file", "-", "--json")
	cmd.Env = job.env
	cmd.Stdin = strings.NewReader(task)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("start shared job %s: %v: %s", job.facts.Name, err, output)
	}
	var facts launchFacts
	if err := json.Unmarshal(output, &facts); err != nil {
		t.Fatalf("decode shared launch: %v: %s", err, output)
	}
	job.facts = facts
	job.socket = socketFromManifest(t, job)
	return facts
}

func registerSharedCleanup(t *testing.T, job *fakeJob) {
	t.Helper()
	t.Cleanup(func() {
		if job.facts.ID == "" {
			entries, _ := os.ReadDir(filepath.Join(job.state, "jobs"))
			for _, entry := range entries {
				var manifest struct {
					Name string `json:"name"`
				}
				content, err := os.ReadFile(filepath.Join(job.state, "jobs", entry.Name(), "manifest.json"))
				if err == nil && json.Unmarshal(content, &manifest) == nil && manifest.Name == job.facts.Name {
					if job.facts.ID != "" {
						t.Logf("multiple uncertain attempts for %s; preserving %s", job.facts.Name, job.root)
						job.sandbox.preserve = true
						return
					}
					job.facts.ID = entry.Name()
				}
			}
		}
		if job.facts.ID != "" {
			if !cleanupFakeJob(t, job) {
				job.sandbox.preserve = true
			}
			return
		}
		if entries, _ := os.ReadDir(filepath.Join(job.state, "jobs")); len(entries) == 0 {
			_ = os.RemoveAll(job.root)
		} else {
			t.Logf("cannot identify owned launch; preserving %s", job.root)
			job.sandbox.preserve = true
		}
	})
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("submission gate was not reached: %s", path)
}

func runCLIJSON(t *testing.T, cli string, env []string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(cli, args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI %v: %v: %s", args, err, output)
	}
	return output
}
