package coordinator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRejectsIncompleteProcessEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Manifest, *Launch)
	}{
		{"missing manifest boot", func(m *Manifest, l *Launch) { m.BootID = "" }},
		{"missing process boot", func(m *Manifest, l *Launch) { l.Server.BootID = "" }},
		{"different process boot", func(m *Manifest, l *Launch) { l.Pane.BootID = "another-boot" }},
		{"zero pid", func(m *Manifest, l *Launch) { l.Server.PID = 0 }},
		{"different uid", func(m *Manifest, l *Launch) { l.Server.UID++ }},
		{"zero birth", func(m *Manifest, l *Launch) { l.Pane.StartSec = 0 }},
		{"invalid microseconds", func(m *Manifest, l *Launch) { l.Pane.StartUsec = 1_000_000 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			boot, err := currentBoot()
			if err != nil {
				t.Fatal(err)
			}
			server := ProcessIdentity{BootID: boot, PID: 123, UID: uint32(os.Getuid()), StartSec: 1}
			pane := ProcessIdentity{BootID: boot, PID: 124, UID: uint32(os.Getuid()), StartSec: 1}
			const id = "123e4567-e89b-42d3-a456-426614174000"
			socket, err := createLifecycleRecord(t, s, id, "evidence", "exec-intent", &server, &pane)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
			manifest, launch, err := s.load(id)
			if err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
			test.change(&manifest, &launch)
			dir := filepath.Join(s.root, "jobs", id)
			if err := writeJSON(dir, "manifest.json", manifest); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(dir, "launch.json", launch); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.load(id); err == nil {
				t.Fatal("accepted incomplete or inconsistent process evidence")
			}
		})
	}
}
