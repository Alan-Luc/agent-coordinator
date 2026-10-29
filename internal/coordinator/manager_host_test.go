package coordinator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"agent-coordinator/internal/pilaunch"
)

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
			managerFile := filepath.Join(root, "manager.jsonl")
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
