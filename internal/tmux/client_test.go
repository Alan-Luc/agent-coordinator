package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestPrivateServerCreateInspectRemove(t *testing.T) {
	for _, locale := range []string{"C", "en_US.UTF-8"} {
		t.Run(locale, func(t *testing.T) {
			t.Setenv("LC_ALL", locale)
			checkPrivateServerCreateInspectRemove(t)
		})
	}
}

func checkPrivateServerCreateInspectRemove(t *testing.T) {
	t.Helper()
	tmuxPath, err := execLookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "ac-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "s")
	configPath := filepath.Join(root, "tmux.conf")
	if err := os.WriteFile(configPath, []byte(Config("owner", "job123", "work-job")), 0600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "launcher")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: tmuxPath}
	session := "ac-job123"
	t.Cleanup(func() { _ = client.Remove(context.Background(), socket, session) })
	if err := client.ValidateVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Create(context.Background(), CreateOptions{Socket: socket, Config: configPath, Session: session, Executable: launcher, JobID: "job123", RuntimeDir: root, Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TERM=xterm-256color", "AC_OWNER=owner", "AC_JOB=job123"}, Width: 100, Height: 30}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Inspect(context.Background(), socket, session)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Owner != "owner" || snapshot.Job != "job123" || snapshot.Protocol != "1" || snapshot.Session != session || snapshot.PaneID == "" || snapshot.Width != 100 || snapshot.Height != 30 {
		t.Fatalf("unexpected private session facts: %+v", snapshot)
	}
	if _, err := client.Capture(context.Background(), socket, snapshot.PaneID); err != nil {
		t.Fatalf("read-only pane capture failed: %v", err)
	}
	afterCapture, err := client.Inspect(context.Background(), socket, session)
	if err != nil || afterCapture.Width != snapshot.Width || afterCapture.Height != snapshot.Height {
		t.Fatalf("capture changed pane geometry: before=%+v after=%+v err=%v", snapshot, afterCapture, err)
	}
	rootKeys, err := client.command(context.Background(), "-S", socket, "-N", "list-keys", "-T", "root")
	if err != nil || strings.TrimSpace(string(rootKeys)) != "bind-key -T root F12 detach-client" {
		t.Fatalf("unexpected root key bindings: %q, %v", rootKeys, err)
	}
	if _, err := client.command(context.Background(), "-S", socket, "-N", "list-keys", "-T", "prefix"); err == nil {
		t.Fatal("default prefix key bindings remain")
	}
	status, err := client.command(context.Background(), "-S", socket, "-N", "show-option", "-gqv", "status-left")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"Observe", "Enter", "work-job", "session_attached"} {
		if !strings.Contains(string(status), fragment) {
			t.Errorf("status missing %q: %q", fragment, status)
		}
	}
	if err := client.Remove(context.Background(), socket, session); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(context.Background(), socket, session); err == nil {
		t.Fatal("inspection succeeded after last session was removed")
	}
}

func TestTransparentConfigLoadsWithoutErrors(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "ac-config-load-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "socket")
	config := filepath.Join(root, "tmux.conf")
	if err := os.WriteFile(config, []byte(TransparentConfig("owner", "job123")), 0600); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: tmuxPath}
	if _, err := client.command(context.Background(), "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "config-test", "/bin/sleep 60"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Remove(context.Background(), socket, "config-test") })
	if output, err := client.command(context.Background(), "-S", socket, "-N", "source-file", config); err != nil {
		t.Fatalf("first config load must not produce an attach error pager: %q, %v", output, err)
	}
}

func TestTransparentViewsShareOnePaneAndRefuseBystanders(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "ac-transparent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "socket")
	config := filepath.Join(root, "tmux.conf")
	if err := os.WriteFile(config, []byte(TransparentConfig("owner", "job123")), 0600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "sleep")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\\nexec /bin/sleep 60\\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: tmuxPath}
	if err := client.Create(context.Background(), CreateOptions{Socket: socket, Config: config, Session: "initial", Executable: launcher, JobID: "job123", RuntimeDir: root, Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TERM=xterm-256color"}, Width: 80, Height: 24}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Remove(context.Background(), socket, "return") })
	t.Cleanup(func() { _ = client.Remove(context.Background(), socket, "initial") })
	if err := client.CreateReturnView(context.Background(), socket, "initial", "return"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.InspectTransparent(context.Background(), socket, "initial", "return")
	if err != nil || snapshot.Protocol != "2" || snapshot.PaneID == "" {
		t.Fatalf("transparent inspection = %+v, %v", snapshot, err)
	}
	keys, err := client.command(context.Background(), "-S", socket, "-N", "list-keys", "-T", "ac-return")
	if err != nil || strings.TrimSpace(string(keys)) != "bind-key -T ac-return F12 detach-client" {
		t.Fatalf("return keys = %q, %v", keys, err)
	}
	if _, err := client.command(context.Background(), "-S", socket, "-N", "list-keys", "-T", "root", "F12"); err == nil {
		t.Fatal("initial view intercepted F12")
	}
	keyTable, err := client.command(context.Background(), "-S", socket, "-N", "show-option", "-t", "return", "key-table")
	if err != nil || strings.TrimSpace(string(keyTable)) != "key-table ac-return" {
		t.Fatalf("return session key table = %q, %v", keyTable, err)
	}
	if snapshot.Clients != 0 {
		t.Fatalf("unexpected aggregate clients: %d", snapshot.Clients)
	}
	if _, err := client.command(context.Background(), "-S", socket, "-N", "new-session", "-d", "-s", "bystander"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectTransparent(context.Background(), socket, "initial", "return"); err == nil {
		t.Fatal("accepted bystander session")
	}
	_, _ = client.command(context.Background(), "-S", socket, "-N", "kill-session", "-t", "=bystander")
	if _, err := client.command(context.Background(), "-S", socket, "-N", "new-window", "-d", "-t", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectTransparent(context.Background(), socket, "initial", "return"); err == nil {
		t.Fatal("accepted an extra window")
	}
}

func TestTransparentPaneExitDetachesBothViews(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "ac-pane-exit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "socket")
	config := filepath.Join(root, "tmux.conf")
	launcher := filepath.Join(root, "launcher")
	exitFile := filepath.Join(root, "exit")
	if err := os.WriteFile(config, []byte(TransparentConfig("owner", "job123")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nwhile [ ! -f \"$AC_EXIT_FILE\" ]; do sleep 0.02; done\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: tmuxPath}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TERM=xterm-256color", "AC_EXIT_FILE=" + exitFile}
	if err := client.Create(context.Background(), CreateOptions{Socket: socket, Config: config, Session: "ac-job123", Executable: launcher, JobID: "job123", RuntimeDir: root, Env: env, Width: 80, Height: 24}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Remove(context.Background(), socket, "ac-job123") })
	if err := client.CreateReturnView(context.Background(), socket, "ac-job123", "ac-return-job123"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Remove(context.Background(), socket, "ac-return-job123") })
	var clients []<-chan error
	for _, session := range []string{"ac-job123", "ac-return-job123"} {
		cmd := client.AttachCommand(context.Background(), socket, session, false)
		cmd.Env = env
		terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = terminal.Close() })
		clients = append(clients, done)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, err := client.InspectTransparent(context.Background(), socket, "ac-job123", "ac-return-job123")
		if err == nil && snapshot.Clients == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("both clients did not attach: %+v, %v", snapshot, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(exitFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, done := range clients {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("pane exit did not detach cleanly: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("pane exit left an attached client stuck")
		}
	}
	snapshot, err := client.InspectTransparent(context.Background(), socket, "ac-job123", "ac-return-job123")
	if err != nil || !snapshot.PaneDead || snapshot.ExitStatus == nil || *snapshot.ExitStatus != 7 || snapshot.Clients != 0 {
		t.Fatalf("exit facts were not retained after detach: %+v, %v", snapshot, err)
	}
}

func TestCaptureUsesPaneTargetWithoutChangingGeometry(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "argv")
	fakeTmux := filepath.Join(root, "tmux")
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\nprintf 'pane output\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: fakeTmux}
	t.Setenv("CAPTURE", capture)
	got, err := client.Capture(context.Background(), "/private/socket", "%7")
	if err != nil || got != "pane output\n" {
		t.Fatalf("Capture() = %q, %v", got, err)
	}
	argv, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(argv)) != "-S\n/private/socket\n-N\ncapture-pane\n-p\n-e\n-t\n%7" {
		t.Fatalf("capture argv = %q", argv)
	}
}

func TestCaptureTailUsesBoundedHistoryRange(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "argv")
	fakeTmux := filepath.Join(root, "tmux")
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\nprintf 'tail output\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: fakeTmux}
	t.Setenv("CAPTURE", capture)
	got, err := client.CaptureTail(context.Background(), "/private/socket", "%7", 25)
	if err != nil || got != "tail output\n" {
		t.Fatalf("CaptureTail() = %q, %v", got, err)
	}
	argv, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "-S\n/private/socket\n-N\ncapture-pane\n-p\n-e\n-S\n-25\n-E\n-1\n-t\n%7"
	if strings.TrimSpace(string(argv)) != want {
		t.Fatalf("CaptureTail argv = %q; want %q", argv, want)
	}
}

func TestCaptureTailRejectsUnboundedLineLimits(t *testing.T) {
	for _, maxLines := range []int{-1, 0, MaxCaptureTailLines + 1} {
		if _, err := (Client{}).CaptureTail(context.Background(), "/socket", "%1", maxLines); err == nil {
			t.Errorf("CaptureTail accepted maxLines=%d", maxLines)
		}
	}
	if _, err := (Client{}).CaptureTail(context.Background(), "", "%1", 1); err == nil {
		t.Fatal("CaptureTail accepted an empty socket")
	}
}

func TestCaptureTailReadsPriorOutputAfterPaneExitWithoutChangingVisibleCapture(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "ac-capture-tail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "socket")
	config := filepath.Join(root, "tmux.conf")
	if err := os.WriteFile(config, []byte(Config("owner", "job123", "tail-test")), 0600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "exit")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf 'early diagnostic marker\\n'\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: tmuxPath}
	if err := client.Create(context.Background(), CreateOptions{Socket: socket, Config: config, Session: "ac-job123", Executable: launcher, JobID: "job123", RuntimeDir: root, Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TERM=xterm-256color"}, Width: 80, Height: 24}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Remove(context.Background(), socket, "ac-job123") })
	deadline := time.Now().Add(3 * time.Second)
	var before Snapshot
	for time.Now().Before(deadline) {
		before, err = client.Inspect(context.Background(), socket, "ac-job123")
		if err == nil && before.PaneDead {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !before.PaneDead {
		t.Fatalf("fixture pane did not exit: %+v, %v", before, err)
	}
	tail, err := client.CaptureTail(context.Background(), socket, before.PaneID, 25)
	if err != nil || !strings.Contains(tail, "early diagnostic marker") {
		t.Fatalf("history tail = %q, %v", tail, err)
	}
	visible, err := client.Capture(context.Background(), socket, before.PaneID)
	if err != nil || !strings.Contains(visible, "Pane is dead") {
		t.Fatalf("visible capture behavior changed: %q, %v", visible, err)
	}
	after, err := client.Inspect(context.Background(), socket, "ac-job123")
	if err != nil || before.Width != after.Width || before.Height != after.Height {
		t.Fatalf("capture changed geometry: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestCaptureRejectsEmptyTarget(t *testing.T) {
	if _, err := (Client{}).Capture(context.Background(), "", "%1"); err == nil {
		t.Fatal("Capture accepted an empty socket")
	}
}

func TestCreateKeepsInternalEnvironmentOutOfArgv(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "argv")
	fakeTmux := filepath.Join(root, "tmux")
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: fakeTmux}
	if err := client.Create(context.Background(), CreateOptions{Socket: "/private/socket", Config: filepath.Join(root, "config"), Session: "ac-job", Executable: "/private/bin/agent", JobID: "job", RuntimeDir: root, Env: []string{"CAPTURE=" + capture, "AC_OWNER=owner", "AC_JOB=job"}, Width: 80, Height: 24}); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "-e\n") || strings.Contains(string(argv), "AC_JOB=") || strings.Contains(string(argv), "AC_OWNER=") {
		t.Fatalf("internal environment leaked into argv: %q", argv)
	}
}

func TestAttachUsesExactReadOnlyTargetWithoutStartingServer(t *testing.T) {
	client := Client{Executable: "/bin/echo"}
	for _, tc := range []struct {
		observe bool
		want    string
	}{{false, "-S /private/socket -N attach-session -E -t =ac-job"}, {true, "-S /private/socket -N attach-session -E -r -t =ac-job"}} {
		cmd := client.AttachCommand(context.Background(), "/private/socket", "ac-job", tc.observe)
		if got := strings.Join(cmd.Args[1:], " "); got != tc.want {
			t.Fatalf("argv %q; want %q", got, tc.want)
		}
	}
}

func TestInspectRejectsMalformedTmuxFactsAndOldVersions(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nif [ \"$1\" = -V ]; then printf 'tmux 3.4\\n'; else printf 'not-facts\\n'; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: fake}
	if err := client.ValidateVersion(context.Background()); err == nil {
		t.Fatal("accepted tmux older than 3.5")
	}
	if _, err := client.Inspect(context.Background(), filepath.Join(root, "missing"), "ac-job"); err == nil {
		t.Fatal("accepted malformed facts")
	}
}

func TestConfigRejectsTmuxConfigInjection(t *testing.T) {
	if got := Config("owner", "job", "safe; run-shell touch /tmp/pwned"); got != "" {
		t.Fatalf("accepted unsafe config token: %q", got)
	}
	config := Config("owner", "job", "safe-name")
	for _, required := range []string{"@ac_owner owner", "@ac_job job", "@ac_protocol 1", "assume-paste-time 0", "extended-keys-format csi-u", "remain-on-exit on", "exit-empty on", "window-size latest", "unbind-key -a -T root", "unbind-key -a -T prefix", "bind-key -T root F12 detach-client", "#{?client_readonly,Observe,Enter}", "safe-name", "#{session_attached}"} {
		if !strings.Contains(config, required) {
			t.Errorf("config missing %q", required)
		}
	}
}

func execLookPath(name string) (string, error) { return exec.LookPath(name) }
