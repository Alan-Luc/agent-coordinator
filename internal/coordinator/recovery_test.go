package coordinator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestForgetRefusesClaimedLegacyRecordEvenWhenMarkedRevoked(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174066"
	live, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	socket, err := createLifecycleRecord(t, s, id, "legacy-retired", "revoked", &live, &live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
	m, l, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	m.BootIDScheme, m.BootID = "", "1791325452.900746"
	l.Server.BootID, l.Pane.BootID = m.BootID, m.BootID
	dir := filepath.Join(s.root, "jobs", id)
	if err := writeJSON(dir, "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "launch.json", l); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.load(id); err != nil {
		t.Fatalf("legacy setup is not readable: %v", err)
	}
	c := &Coordinator{store: s}
	err = c.Forget(t.Context(), id, true)
	var action *ActionError
	if !errors.As(err, &action) || action.Diagnostic.Code != "legacy_identity" {
		t.Fatalf("ambiguous claimed legacy work was not refused: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "initial-task.txt"))
	if err != nil || string(body) != "task" {
		t.Fatalf("legacy task content was deleted: %q %v", body, err)
	}
}

func TestClaimedLegacyRetiredFlagCannotReleaseName(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174068"
	live, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	socket, err := createLifecycleRecord(t, s, id, "legacy-name", "revoked", &live, &live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
	m, l, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	m.BootIDScheme, m.BootID = "", "1791325452.900746"
	l.Server.BootID, l.Pane.BootID = m.BootID, m.BootID
	dir := filepath.Join(s.root, "jobs", id)
	if err := writeJSON(dir, "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "launch.json", l); err != nil {
		t.Fatal(err)
	}
	fakeTmux := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nif [ \"$1\" = -V ]; then printf 'tmux 3.5\\n'; exit 0; fi\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s, options: Options{PiPath: "/usr/bin/true", TmuxPath: fakeTmux, Executable: "/usr/bin/true"}}
	t.Run("resolve-visible-legacy-name", func(t *testing.T) {
		got, err := c.resolve(m.Name, false)
		if err != nil || got != id {
			t.Fatalf("uncertain legacy name silently released: id=%s err=%v", got, err)
		}
	})
	t.Run("refuse-name-reuse", func(t *testing.T) {
		_, err := c.Start(t.Context(), m.Name, t.TempDir(), []byte("must not launch"))
		var action *ActionError
		if !errors.As(err, &action) || action.Diagnostic.Code != "duplicate_name" {
			t.Fatalf("legacy retired flag authorized name reuse: %v", err)
		}
	})
	t.Run("refuse-already-ambiguous-name", func(t *testing.T) {
		if _, err := createLifecycleRecord(t, s, "123e4567-e89b-42d3-a456-426614174069", m.Name, "preexec", nil, nil); err != nil {
			t.Fatal(err)
		}
		_, err := c.resolve(m.Name, false)
		var action *ActionError
		if !errors.As(err, &action) || action.Diagnostic.Code != "ambiguous_name" {
			t.Fatalf("ambiguous legacy name selected a different UUID: %v", err)
		}
	})
}

func TestEndedControllerCleanupPreservesUnprovenEndpoints(t *testing.T) {
	for _, kind := range []string{"live-listener", "ordinary-file"} {
		t.Run(kind, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			process := exec.Command("/bin/sleep", "0.2")
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			pane, err := identifyProcess(process.Process.Pid)
			if err != nil {
				_ = process.Wait()
				t.Fatal(err)
			}
			if err := process.Wait(); err != nil {
				t.Fatal(err)
			}
			id := "123e4567-e89b-42d3-a456-426614174067"
			socket, err := createLifecycleRecord(t, s, id, "endpoint-proof", "revoked", nil, &pane)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
			m, l, err := s.load(id)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := controlEndpoint(socket)
			if kind == "live-listener" {
				listener, err := net.Listen("unix", endpoint)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			} else if err := os.WriteFile(endpoint, []byte("not a controller socket"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err := removeEndedControlEndpoint(t.Context(), m, l); err == nil {
				t.Fatalf("cleanup accepted %s", kind)
			}
			after, err := os.Lstat(endpoint)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("cleanup removed unproven %s: %v", kind, err)
			}
		})
	}
}

func TestForgetRevokedResidueByIDAfterNameReuse(t *testing.T) {
	assertForgetNoIdentityResidue(t, "revoked", true)
}

func TestForgetFailedNoIdentityDeadResidue(t *testing.T) {
	assertForgetNoIdentityResidue(t, "failed", false)
}

func assertForgetNoIdentityResidue(t *testing.T, phase string, reuseName bool) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	oldID := "123e4567-e89b-42d3-a456-426614174000"
	newID := "123e4567-e89b-42d3-a456-426614174001"
	oldSocket, err := createLifecycleRecord(t, s, oldID, "reused", phase, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(oldSocket))) })
	server := startIdentityFixture(t, oldSocket)
	if reuseName {
		if _, err := createLifecycleRecord(t, s, newID, "reused", "preexec", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	fakeTmux, removed := fakeTmuxForSession(t, s.token, oldID, server.pid)
	m, l, err := s.load(oldID)
	if err != nil || l.Phase != phase || l.Server != nil || l.Pane != nil {
		t.Fatalf("fixture lacks expected identity-free phase: %#v %#v %v", m, l, err)
	}
	m.Tmux = fakeTmux
	if err := writeJSON(filepath.Join(root, "jobs", oldID), "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	if snap, err := c.ownedSnapshot(context.Background(), m); err != nil {
		t.Fatalf("fake owned snapshot: %v", err)
	} else if snap.ServerPID != server.pid || !snap.PaneDead {
		t.Fatalf("unexpected fake snapshot: %+v", snap)
	}
	if err := c.Forget(context.Background(), oldID, true); err != nil {
		t.Fatalf("forget late dead residue: %v", err)
	}
	if _, err := os.Stat(removed); err != nil {
		t.Fatalf("exact dead session was not removed: %v", err)
	}
	_, retired, err := s.load(oldID)
	if err != nil || retired.Server == nil || retired.Pane != nil || retired.Phase != "revoked" {
		t.Fatalf("server-only cleanup identity was not retained: %#v %v", retired, err)
	}

	if err := c.Forget(context.Background(), oldID, true); err != nil {
		t.Fatalf("idempotent forget failed: %v", err)
	}
	if _, err := os.Lstat(oldSocket); !os.IsNotExist(err) {
		t.Fatalf("stale socket remains: %v", err)
	}
	_, retired, err = s.load(oldID)
	if err != nil || retired.Phase != "revoked" || !retired.Retired {
		t.Fatalf("tombstone was not preserved: %#v %v", retired, err)
	}
	if reuseName {
		_, current, err := s.load(newID)
		if err != nil || current.Phase != "preexec" || current.Retired {
			t.Fatalf("same-name active job was affected: %#v %v", current, err)
		}
	}
}

func TestFactsClassifyStaleSocketWithLivePaneAsLostTerminal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174004"
	socket, err := s.socket(id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	server, pane := identity, identity
	server.StartSec++
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })

	if _, err := createLifecycleRecord(t, s, id, "lost-terminal", "exec-intent", &server, &pane); err != nil {
		t.Fatal(err)
	}
	m, l, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(root, "jobs", id), "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	facts := c.facts(context.Background(), m, l)
	if facts.Launch != "lost-terminal" || facts.Terminal != "lost" {
		t.Fatalf("ended server/live pane was not kept as lost-terminal: %+v", facts)
	}
}

func TestFactsDoNotOfferRepairForStaleSocketWithLiveServer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174006"
	socket, err := s.socket(id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
	if _, err := createLifecycleRecord(t, s, id, "stale-live-server", "exec-intent", &identity, &identity); err != nil {
		t.Fatal(err)
	}
	m, l, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	facts := c.facts(context.Background(), m, l)
	if facts.Launch != "unknown" || facts.NextAction == "repair-socket after confirming" {
		t.Fatalf("stale socket with live server was offered repair: %+v", facts)
	}
}

func TestFactsDoNotReconcileStaleSocketWithForeignListener(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174005"
	socket, err := s.socket(id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	server, pane := identity, identity
	server.StartSec++
	pane.StartSec++
	foreign, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })

	if _, err := createLifecycleRecord(t, s, id, "foreign-listener", "exec-intent", &server, &pane); err != nil {
		t.Fatal(err)
	}
	m, l, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(root, "jobs", id), "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	facts := c.facts(context.Background(), m, l)
	if facts.Launch != "unknown" || facts.Terminal != "unknown" {
		t.Fatalf("foreign listener was treated as a stale socket: %+v", facts)
	}
}

func TestForgetKeepsStaleSocketWithLiveForeignListener(t *testing.T) {
	c, _, id, socket, _ := createRemovedResidue(t)
	foreign, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if err := c.Forget(context.Background(), id, true); err == nil {
		t.Fatal("Forget removed a socket bound by a foreign live listener")
	}
	if info, err := os.Lstat(socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("foreign listener socket was removed or changed: %v %v", info, err)
	}
}

func TestForgetRejectsMismatchedRecordedServerIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174002"
	socket, err := s.socket(id, true)
	if err != nil {
		t.Fatal(err)
	}
	server := startIdentityFixture(t, socket)
	identity, err := identifyProcess(server.pid)
	if err != nil {
		t.Fatal(err)
	}
	identity.StartSec++
	socket, err = createLifecycleRecord(t, s, id, "mismatch", "revoked", &identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
	fakeTmux, removed := fakeTmuxForSession(t, s.token, id, server.pid)
	m, _, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	m.Tmux = fakeTmux
	if err := writeJSON(filepath.Join(root, "jobs", id), "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	if err := c.Forget(context.Background(), id, true); err == nil {
		t.Fatal("Forget accepted a mismatched server birth identity")
	}
	if _, err := os.Stat(removed); !os.IsNotExist(err) {
		t.Fatalf("session removal was attempted: %v", err)
	}
}

func createRemovedResidue(t *testing.T) (*Coordinator, *store, string, string, *identityFixture) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174003"
	socket, err := createLifecycleRecord(t, s, id, "removed", "revoked", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
	server := startIdentityFixture(t, socket)
	fakeTmux, _ := fakeTmuxForSession(t, s.token, id, server.pid)
	m, _, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	m.Tmux = fakeTmux
	if err := writeJSON(filepath.Join(root, "jobs", id), "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	if err := c.Forget(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	return c, s, id, socket, server
}

func fakeTmuxForSession(t *testing.T, owner, id string, serverPID int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "tmux")
	marker := filepath.Join(dir, "removed")
	removed := filepath.Join(dir, "remove.log")
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n *' display-message '*) [ ! -e '%s' ] || exit 44; printf '%%s|%%s|1|ac-%%s|%%%%0|%%d|%%d|1|0|0|80|24\\n' '%s' '%s' '%s' %d %d ;;\n *' kill-session '*) kill -TERM %d; : > '%s'; printf '%%s\\n' \"$*\" >> '%s' ;;\n *) exit 44 ;;\nesac\n", marker, owner, id, id, serverPID, os.Getpid(), serverPID, marker, removed)
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return fake, removed
}

type identityFixture struct {
	pid  int
	cmd  *exec.Cmd
	done chan error
}

func TestUnixIdentityFixtureProcess(t *testing.T) {
	socket := os.Getenv("AC_TEST_UNIX_SOCKET")
	if socket == "" {
		return
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	<-signals
}

func startIdentityFixture(t *testing.T, socket string) *identityFixture {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnixIdentityFixtureProcess$")
	cmd.Env = append(os.Environ(), "AC_TEST_UNIX_SOCKET="+socket)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &identityFixture{pid: cmd.Process.Pid, cmd: cmd, done: make(chan error, 1)}
	go func() { fixture.done <- cmd.Wait() }()
	t.Cleanup(func() { stopIdentityFixture(t, fixture) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socket, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return fixture
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("identity fixture did not bind its socket")
	return nil
}

func stopIdentityFixture(t *testing.T, fixture *identityFixture) {
	t.Helper()
	select {
	case <-fixture.done:
		return
	default:
		_ = fixture.cmd.Process.Kill()
		<-fixture.done
	}
}

func TestContinueRequiresMatchingServerAndForegroundGroup(t *testing.T) {
	server := ProcessIdentity{BootID: "boot", PID: 10, UID: 20, StartSec: 30}
	pane := ProcessIdentity{BootID: "boot", PID: 11, UID: 20, StartSec: 31}
	if !continueIdentityMatches(server, pane, server, pane, pane.PID, pane.PID) {
		t.Fatal("matching foreground identity rejected")
	}
	otherServer := server
	otherServer.StartSec++
	if continueIdentityMatches(server, pane, otherServer, pane, pane.PID, pane.PID) {
		t.Fatal("Continue accepted a different server birth identity")
	}
	if continueIdentityMatches(server, pane, server, pane, pane.PID, pane.PID+1) {
		t.Fatal("Continue accepted a foreground group other than the pane PID")
	}
}

func createLifecycleRecord(t *testing.T, s *store, id, name, phase string, server, pane *ProcessIdentity) (string, error) {
	t.Helper()
	socket, err := s.socket(id, true)
	if err != nil {
		return "", err
	}
	boot, err := currentBoot()
	if err != nil {
		return "", err
	}
	m := Manifest{Schema: SchemaVersion, ID: id, Name: name, Owner: s.token, Cwd: t.TempDir(), Pi: "/bin/true", Tmux: "/bin/true", Coordinator: "/bin/true", BootID: boot, BootIDScheme: BootIdentityScheme, Protocol: ProtocolVersion, Socket: socket}
	dir := filepath.Join(s.root, "jobs", id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	if err := writeJSON(dir, "manifest.json", m); err != nil {
		return "", err
	}
	if err := writeJSON(dir, "launch.json", Launch{Schema: SchemaVersion, Phase: phase, Server: server, Pane: pane, Retired: phase == "revoked"}); err != nil {
		return "", err
	}
	if err := writeBytes(dir, "initial-task.txt", []byte("task")); err != nil {
		return "", err
	}
	f, err := privateFile(filepath.Join(dir, "launch.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return socket, nil
}
