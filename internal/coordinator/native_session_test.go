package coordinator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativeObservationCorrelatesExactTargetAndFile(t *testing.T) {
	for _, test := range []struct {
		name      string
		persisted bool
		mutate    func(*NativeSessionObservation, string)
		wantError bool
	}{
		{name: "planned unpersisted path"},
		{name: "persisted0644", persisted: true},
		{name: "old generation", mutate: func(n *NativeSessionObservation, _ string) { n.Generation = "123e4567-e89b-42d3-a456-426614174099" }, wantError: true},
		{name: "old birth", mutate: func(n *NativeSessionObservation, _ string) { n.Process.StartUsec++ }, wantError: true},
		{name: "stale observation", mutate: func(n *NativeSessionObservation, _ string) { n.UpdatedAt = n.UpdatedAt.Add(-time.Minute) }, wantError: true},
		{name: "future observation", mutate: func(n *NativeSessionObservation, _ string) { n.UpdatedAt = n.UpdatedAt.Add(time.Minute) }, wantError: true},
		{name: "unpersisted claim for existing file", persisted: true, mutate: func(n *NativeSessionObservation, _ string) { n.State = "unpersisted"; n.File = nil; n.HeaderID = "" }, wantError: true},
		{name: "wrong inode", persisted: true, mutate: func(n *NativeSessionObservation, _ string) { n.File.Inode++ }, wantError: true},
		{name: "malicious writable file", persisted: true, mutate: func(n *NativeSessionObservation, path string) {
			if err := os.Chmod(path, 0666); err != nil {
				panic(err)
			}
		}, wantError: true},
		{name: "wrong header", persisted: true, mutate: func(n *NativeSessionObservation, path string) {
			if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"different"}`+"\n"), 0644); err != nil {
				panic(err)
			}
		}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			cwd := t.TempDir()
			if canonical, err := filepath.EvalSymlinks(cwd); err == nil {
				cwd = canonical
			} else {
				t.Fatal(err)
			}
			file := filepath.Join(cwd, "conversation.jsonl")
			now := time.Now().UTC()
			const job = "123e4567-e89b-42d3-a456-426614174078"
			const session = "123e4567-e89b-42d3-a456-426614174079"
			const generation = "123e4567-e89b-42d3-a456-426614174080"
			birth := ProcessIdentity{BootID: "123e4567-e89b-42d3-a456-426614174081", PID: 123, UID: uint32(os.Getuid()), StartSec: 1}
			manifest := Manifest{ID: job, Owner: "0123456789abcdef", Protocol: InteractiveProtocolVersion}
			launch := Launch{Pane: &birth}
			status := SessionStatus{Protocol: 1, LaunchProtocol: "2", JobID: job, Owner: manifest.Owner, SessionID: session, Generation: generation, PID: birth.PID, UpdatedAt: now.UnixMilli(), Activity: "idle"}
			observation := NativeSessionObservation{Schema: 1, JobID: job, Owner: manifest.Owner, SessionID: session, Generation: generation, Process: birth, UpdatedAt: now, SessionFile: file, Cwd: cwd, State: "unpersisted"}
			if test.persisted {
				if err := writeJSON(cwd, "conversation.jsonl", struct {
					Type    string `json:"type"`
					Version int    `json:"version"`
					ID      string `json:"id"`
					Cwd     string `json:"cwd"`
				}{"session", 3, session, cwd}); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
				var stat unix.Stat_t
				if err := unix.Lstat(file, &stat); err != nil {
					t.Fatal(err)
				}
				observation.State = "persisted"
				observation.File = &NativeFileIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}
				observation.HeaderID = session
			}
			if test.mutate != nil {
				test.mutate(&observation, file)
			}
			if err := writeJSON(dir, "native-session.json", observation); err != nil {
				t.Fatal(err)
			}
			_, err := readNativeSessionForTarget(dir, manifest, launch, status, now)
			if (err != nil) != test.wantError {
				t.Fatalf("native observation error=%v, wantError=%v", err, test.wantError)
			}
		})
	}
}
