package coordinator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPreservesLegacyIdentityWithoutTrustingUnknownScheme(t *testing.T) {
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
	_, err = createLifecycleRecord(t, s, id, "legacy-worker", "exec-intent", &server, &pane)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "jobs", id)
	var manifest Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.BootID = "1791325452.900746"
	manifest.BootIDScheme = ""
	server.BootID, pane.BootID = manifest.BootID, manifest.BootID
	if err := writeJSON(dir, "manifest.json", manifest); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "launch.json", Launch{Schema: SchemaVersion, Phase: "exec-intent", Server: &server, Pane: &pane}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.load(id); err != nil {
		t.Fatalf("valid legacy reservation rejected: %v", err)
	}
	c := &Coordinator{store: s}
	facts := c.facts(t.Context(), manifest, Launch{Schema: SchemaVersion, Phase: "exec-intent", Server: &server, Pane: &pane})
	if facts.Launch != "unknown" || facts.Terminal != "unknown" {
		t.Fatalf("legacy record was not kept unknown and visible: %+v", facts)
	}
	launchPath := filepath.Join(dir, "launch.json")
	launchBefore, err := os.ReadFile(launchPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_JOB", id)
	t.Setenv("AC_STATE_DIR", s.root)
	t.Setenv("AC_OWNER", s.token)
	if err := RunBootstrap(t.Context(), id); err == nil {
		t.Fatal("legacy boot identity authorized bootstrap")
	}
	launchAfter, err := os.ReadFile(launchPath)
	if err != nil || string(launchAfter) != string(launchBefore) {
		t.Fatalf("legacy bootstrap rewrote launch evidence: err=%v", err)
	}
	manifest.BootIDScheme = "unknown-scheme"
	if err := writeJSON(dir, "manifest.json", manifest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.load(id); err == nil {
		t.Fatal("unknown boot identity scheme accepted")
	}
}

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
