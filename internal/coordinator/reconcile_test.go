package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestForgetRevokesPreexecBeforeRemovingTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174000"
	socket, err := s.socket(id, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(socket))) })
	m := Manifest{Schema: SchemaVersion, ID: id, Name: "job", Owner: s.token, Cwd: t.TempDir(), Pi: "/bin/true", Tmux: "/bin/true", Coordinator: "/bin/true", BootID: "testboot", Protocol: ProtocolVersion, Socket: socket}
	dir := filepath.Join(root, "jobs", id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "launch.json", Launch{Schema: SchemaVersion, Phase: "preexec"}); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(dir, "initial-task.txt", []byte("task")); err != nil {
		t.Fatal(err)
	}
	f, err := privateFile(filepath.Join(dir, "launch.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	c := &Coordinator{store: s}
	if err := c.Forget(context.Background(), "job", false); err != nil {
		t.Fatal(err)
	}
	_, l, err := s.load(id)
	if err != nil || l.Phase != "revoked" || !l.Retired {
		t.Fatalf("not durably revoked: %#v %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "initial-task.txt")); !os.IsNotExist(err) {
		t.Fatalf("task remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", id, "launch.lock")); err != nil {
		t.Fatalf("launch lock removed: %v", err)
	}
}

func TestFactsDoNotClaimReadinessOrTaskSuccess(t *testing.T) {
	c := &Coordinator{store: &store{token: "0123456789abcdef"}}
	m := Manifest{Schema: SchemaVersion, ID: "123e4567-e89b-42d3-a456-426614174000", Name: "job", Owner: c.store.token, Protocol: ProtocolVersion, BootID: "boot"}
	f := c.facts(context.Background(), m, Launch{Schema: SchemaVersion, Phase: "preexec"})
	if f.Launch != "starting" || f.PiReadiness != "unverified" || f.TaskOutcome != "unknown" {
		t.Fatalf("unexpected facts: %#v", f)
	}
}
