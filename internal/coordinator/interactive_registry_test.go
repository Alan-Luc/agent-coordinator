package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"agent-coordinator/internal/pilaunch"
)

func TestLoadInteractiveRequiresIntentAndRole(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    func(string)
		wantError bool
	}{
		{name: "complete interactive record"},
		{name: "missing intent", change: func(dir string) { _ = os.Remove(filepath.Join(dir, "intent.json")) }, wantError: true},
		{name: "missing role", change: func(dir string) { _ = os.Remove(filepath.Join(dir, "role.json")) }, wantError: true},
		{name: "mixed task", change: func(dir string) {
			if err := writeBytes(dir, "initial-task.txt", []byte("task")); err != nil {
				panic(err)
			}
		}, wantError: true},
		{name: "unknown role", change: func(dir string) {
			if err := writeJSON(dir, "role.json", map[string]any{"schema": 1, "role": "worker-and-manager"}); err != nil {
				panic(err)
			}
		}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			const id = "123e4567-e89b-42d3-a456-426614174078"
			socket, err := createLifecycleRecord(t, s, id, "interactive", "preexec", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
			dir := filepath.Join(s.root, "jobs", id)
			manifest, _, err := s.load(id)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Protocol = "2"
			if err := writeJSON(dir, "manifest.json", manifest); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "initial-task.txt")); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"schema":1,"argv":[]}`)
			hash := sha256.Sum256(body)
			if err := writeJSON(dir, "intent.json", InteractiveIntent{Schema: 1, Kind: "interactive", TerminalProfile: "transparent-v1", ArgvSHA256: hex.EncodeToString(hash[:]), NativePi: &pilaunch.Managed{Executable: "/bin/pi", Observation: pilaunch.Observation{Version: "1.4.0"}}}); err != nil {
				t.Fatal(err)
			}
			if err := writeBytes(dir, "launch-argv.json", body); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(dir, "role.json", map[string]any{"schema": 1, "role": "standalone"}); err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(dir)
			}
			_, _, err = s.load(id)
			if (err != nil) != test.wantError {
				t.Fatalf("load error=%v, wantError=%v", err, test.wantError)
			}
		})
	}
}
