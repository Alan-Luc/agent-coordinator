package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-coordinator/internal/pilaunch"
	"golang.org/x/sys/unix"
)

func TestStandaloneManagerReconnectDoesNotRequireManagerHostRole(t *testing.T) {
	root, err := canonicalCwd(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	const id = "123e4567-e89b-42d3-a456-426614174084"
	directory := filepath.Join(store.root, "jobs", id)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	socket, err := store.socket(id, true)
	if err != nil {
		t.Fatal(err)
	}
	managed := interactiveManagedFixture(t)
	manifest := Manifest{Schema: SchemaVersion, ID: id, Name: "standalone", Owner: store.token, Cwd: root, Pi: managed.Executable, Tmux: "/bin/tmux", Coordinator: "/bin/coordinator", Created: time.Now(), BootID: identity.BootID, BootIDScheme: BootIdentityScheme, Protocol: InteractiveProtocolVersion, Socket: socket}
	if err := writeJSON(directory, "manifest.json", manifest); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(directory, "launch.json", Launch{Schema: SchemaVersion, Phase: "exec-intent", Server: &identity, Pane: &identity}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(directory, "intent.json", InteractiveIntent{Schema: SchemaVersion, Kind: "interactive", TerminalProfile: TransparentTerminalProfile, ArgvSHA256: strings.Repeat("a", 64), NativePi: &managed}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(directory, "role.json", JobRole{Schema: SchemaVersion, Role: "standalone"}); err != nil {
		t.Fatal(err)
	}
	sessionID := "123e4567-e89b-42d3-a456-426614174085"
	sessionFile := filepath.Join(root, "native.jsonl")
	if err := os.WriteFile(sessionFile, []byte(fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":%q}`+"\n", sessionID, root)), 0600); err != nil {
		t.Fatal(err)
	}
	var fileInfo unix.Stat_t
	if err := unix.Lstat(sessionFile, &fileInfo); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	status := SessionStatus{Protocol: ControlProtocol, LaunchProtocol: InteractiveProtocolVersion, JobID: id, Owner: store.token, SessionID: sessionID, Generation: "123e4567-e89b-42d3-a456-426614174086", PID: os.Getpid(), UpdatedAt: now.UnixMilli(), Activity: "idle"}
	if err := writeJSON(directory, "session.json", status); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(directory, "native-session.json", NativeSessionObservation{Schema: SchemaVersion, JobID: id, Owner: store.token, SessionID: sessionID, Generation: status.Generation, Process: identity, UpdatedAt: now, SessionFile: sessionFile, Cwd: root, State: "persisted", File: &NativeFileIdentity{Device: uint64(fileInfo.Dev), Inode: fileInfo.Ino}, HeaderID: sessionID}); err != nil {
		t.Fatal(err)
	}
	caller := CommunicationCaller{SessionFile: sessionFile, SessionID: sessionID, Generation: status.Generation, PID: os.Getpid()}
	c := &Coordinator{store: store}
	_, err = c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "reconnect", Caller: &caller}, os.Getpid())
	var action *ActionError
	if !errors.As(err, &action) || action.Diagnostic.Code != "manager_unknown" {
		t.Fatalf("standalone reconnect must return definitive manager_unknown, got %v (conversation=%v)", err, validManagerConversation(caller))
	}
}

func TestManagerHostPromotionIsStickyAndRefusesWorkerBinding(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone promotion", true: "bound worker refusal"}[bound], func(t *testing.T) {
			tmuxPath, err := exec.LookPath("tmux")
			if err != nil {
				t.Skip("tmux unavailable")
			}
			root, err := canonicalCwd(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			stop := filepath.Join(root, "stop")
			ready := filepath.Join(root, "ready")
			t.Setenv("ROLE_FIXTURE_STOP", stop)
			t.Setenv("ROLE_FIXTURE_READY", ready)
			managed := interactiveManagedFixture(t)
			fixture := "#!/usr/bin/env node\n" + `const fs=require('node:fs'); fs.writeFileSync(process.env.ROLE_FIXTURE_READY,String(process.pid)); setInterval(()=>{if(fs.existsSync(process.env.ROLE_FIXTURE_STOP))process.exit(0)},25);`
			if err := os.WriteFile(managed.Observation.Entry.Path, []byte(fixture), 0700); err != nil {
				t.Fatal(err)
			}
			managed, err = pilaunch.ObserveManaged(managed.Executable)
			if err != nil {
				t.Fatal(err)
			}
			s, err := openStore(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			c := &Coordinator{store: s, options: Options{Executable: self, TmuxPath: tmuxPath}}
			facts, err := c.StartInteractive(t.Context(), root, []string{}, managed)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.WriteFile(stop, []byte("stop"), 0600)
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					f, e := c.DoctorID(context.Background(), facts.ID)
					if e == nil && f.Launch == "exited" {
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if state, err := c.communicationLoad(); err == nil {
					bindings := state.Bindings[:0]
					for _, binding := range state.Bindings {
						if binding.JobID != facts.ID {
							bindings = append(bindings, binding)
						}
					}
					state.Bindings = bindings
					if err := c.communicationSave(state); err != nil {
						t.Errorf("fixture unbinding: %v", err)
					}
				} else {
					t.Errorf("fixture binding state: %v", err)
				}
				if err := c.Forget(ctx, facts.ID, true); err != nil {
					t.Errorf("owned fixture cleanup: %v", err)
				}
			})
			m, l, err := s.load(facts.ID)
			if err != nil {
				t.Fatal(err)
			}
			readyDeadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(readyDeadline) {
				if body, err := os.ReadFile(ready); err == nil && string(body) == fmt.Sprint(l.Pane.PID) {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if body, err := os.ReadFile(ready); err != nil || string(body) != fmt.Sprint(l.Pane.PID) {
				t.Fatalf("fixture did not reach readiness: %q %v", body, err)
			}
			const session = "123e4567-e89b-42d3-a456-426614174079"
			const generation = "123e4567-e89b-42d3-a456-426614174080"
			now := time.Now().UTC()
			status := SessionStatus{Protocol: 1, LaunchProtocol: "2", JobID: m.ID, Owner: m.Owner, SessionID: session, Generation: generation, PID: l.Pane.PID, UpdatedAt: now.UnixMilli(), Activity: "idle"}
			dir := filepath.Join(s.root, "jobs", facts.ID)
			if err := writeJSON(dir, "session.json", status); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(dir, "native-session.json", NativeSessionObservation{Schema: 1, JobID: m.ID, Owner: m.Owner, SessionID: session, Generation: generation, Process: *l.Pane, UpdatedAt: now, SessionFile: filepath.Join(root, "planned.jsonl"), Cwd: root, State: "unpersisted"}); err != nil {
				t.Fatal(err)
			}
			if bound {
				state, err := c.communicationLoad()
				if err != nil {
					t.Fatal(err)
				}
				state.Managers = append(state.Managers, CommunicationManager{ID: "123e4567-e89b-42d3-a456-426614174081", SessionFile: "/fixture/other.jsonl", SessionID: "other"})
				state.Bindings = append(state.Bindings, CommunicationBinding{JobID: m.ID, ManagerID: state.Managers[0].ID})
				if err := c.communicationSave(state); err != nil {
					t.Fatal(err)
				}
			}
			target := SessionTarget{ID: m.ID, SessionID: session, Generation: generation, Pane: *l.Pane}
			role, err := c.ReserveManagerHost(t.Context(), target, l.Pane.PID)
			if bound {
				if err == nil {
					t.Fatal("bound worker promoted to manager")
				}
				return
			}
			if err != nil || role.Role != "manager-host" {
				t.Fatalf("standalone host promotion=%+v error=%v", role, err)
			}
			if role, err = c.ReserveManagerHost(t.Context(), target, l.Pane.PID); err != nil || role.Role != "manager-host" {
				fresh, targetErr := c.ControlTarget(t.Context(), m.ID)
				_, _, loadErr := s.load(m.ID)
				_, nativeErr := readNativeSessionForTarget(dir, m, l, status, time.Now())
				t.Fatalf("same-host reservation was not idempotent: %+v %v; target=%+v targetErr=%v loadErr=%v nativeErr=%v", role, err, fresh, targetErr, loadErr, nativeErr)
			}
			if _, err = c.ReserveManagerHost(t.Context(), target, l.Pane.PID+1); err == nil {
				t.Fatal("non-parent promoted host")
			}
			persisted, err := readJobRole(dir)
			if err != nil || persisted.Role != "manager-host" {
				t.Fatalf("role was not sticky: %+v %v", persisted, err)
			}
			managerFile := filepath.Join(root, "planned.jsonl")
			if err := os.WriteFile(managerFile, []byte(fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":%q}`+"\n", session, root)), 0600); err != nil {
				t.Fatal(err)
			}
			var nativeFileInfo unix.Stat_t
			if err := unix.Lstat(managerFile, &nativeFileInfo); err != nil {
				t.Fatal(err)
			}
			status.Generation = "123e4567-e89b-42d3-a456-426614174087"
			if err := writeJSON(dir, "session.json", status); err != nil {
				t.Fatal(err)
			}
			native := NativeSessionObservation{Schema: SchemaVersion, JobID: m.ID, Owner: m.Owner, SessionID: session, Generation: status.Generation, Process: *l.Pane, UpdatedAt: now, SessionFile: managerFile, Cwd: root, State: "persisted", File: &NativeFileIdentity{Device: uint64(nativeFileInfo.Dev), Inode: nativeFileInfo.Ino}, HeaderID: session}
			if err := writeJSON(dir, "native-session.json", native); err != nil {
				t.Fatal(err)
			}
			managerCaller := &CommunicationCaller{SessionFile: managerFile, SessionID: session, Generation: status.Generation, PID: l.Pane.PID}
			if _, err := c.Communication(t.Context(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "enroll", Caller: managerCaller}, l.Pane.PID); err != nil {
				t.Fatalf("backend enrollment: %v", err)
			}
			if err := os.Remove(filepath.Join(dir, "session.json")); err != nil {
				t.Fatal(err)
			}
			staleCaller := *managerCaller
			staleCaller.Generation = "123e4567-e89b-42d3-a456-426614174099"
			_, staleErr := c.Communication(t.Context(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "disconnect", Caller: &staleCaller}, l.Pane.PID)
			var staleAction *ActionError
			if !errors.As(staleErr, &staleAction) || staleAction.Diagnostic.Code != "lease_mismatch" {
				t.Fatalf("stale manager generation could disconnect: %v", staleErr)
			}
			if _, err := c.Communication(t.Context(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "disconnect", Caller: managerCaller}, l.Pane.PID); err != nil {
				t.Fatalf("exact live-birth disconnect should release its lease after controller status teardown: %v", err)
			}
			state, err := c.communicationLoad()
			if err != nil || state.Managers[0].Lease != nil {
				t.Fatalf("disconnect did not release the exact backend lease: %+v %v", state.Managers, err)
			}
			status.Generation = "123e4567-e89b-42d3-a456-426614174088"
			status.UpdatedAt = time.Now().UnixMilli()
			native.Generation = status.Generation
			native.UpdatedAt = time.Now()
			if err := writeJSON(dir, "session.json", status); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(dir, "native-session.json", native); err != nil {
				t.Fatal(err)
			}
			newCaller := &CommunicationCaller{SessionFile: managerFile, SessionID: session, Generation: status.Generation, PID: l.Pane.PID}
			result, err := c.Communication(t.Context(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "reconnect", Caller: newCaller}, l.Pane.PID)
			if err != nil || result.Manager == nil || result.Manager.Lease == nil || result.Manager.Lease.Generation != newCaller.Generation {
				t.Fatalf("reload could not reconnect after exact prior-generation disconnect: %+v %v", result, err)
			}
			managerFile = filepath.Join(root, "manager.jsonl")
			if err := writeJSON(root, "manager.jsonl", map[string]any{"type": "session", "version": 3, "id": "fixture-manager", "cwd": root}); err != nil {
				t.Fatal(err)
			}
			caller := &CommunicationCaller{SessionFile: managerFile, SessionID: "fixture-manager", Generation: "123e4567-e89b-42d3-a456-426614174083", PID: os.Getpid()}
			if _, err := c.Communication(t.Context(), CommunicationRequest{Protocol: 1, Action: "enroll", Caller: caller}, os.Getpid()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Communication(t.Context(), CommunicationRequest{Protocol: 1, Action: "adopt", Caller: caller, JobID: m.ID, Target: &target}, os.Getpid()); err == nil {
				t.Fatal("sticky manager host was adopted as a worker")
			}
		})
	}
}
