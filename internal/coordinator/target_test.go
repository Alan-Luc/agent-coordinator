package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCachedUUIDActionsDoNotTargetReusedName(t *testing.T) {
	t.Setenv("TMUX", "")
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	const oldID = "123e4567-e89b-42d3-a456-426614174000"
	const newID = "123e4567-e89b-42d3-a456-426614174001"
	if _, err := createLifecycleRecord(t, s, oldID, "worker", "preexec", nil, nil); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	cached, err := c.Doctor(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(context.Background(), "worker", false); err != nil {
		t.Fatal(err)
	}
	if _, err := createLifecycleRecord(t, s, newID, "worker", "preexec", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{"continue", func() error { return c.ContinueID(context.Background(), cached.ID) }},
		{"repair", func() error { return c.RepairSocketID(context.Background(), cached.ID) }},
		{"enter", func() error { _, err := c.AttachID(context.Background(), cached.ID, false); return err }},
		{"observe", func() error { _, err := c.AttachID(context.Background(), cached.ID, true); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			var action *ActionError
			if !errors.As(err, &action) || action.Diagnostic.ID != oldID {
				t.Fatalf("stale action must refuse the original UUID, got %v", err)
			}
		})
	}
	inspected, err := c.DoctorID(context.Background(), cached.ID)
	if err != nil || inspected.ID != oldID || !inspected.Retired {
		t.Fatalf("stale inspection = %#v, %v", inspected, err)
	}
	if err := c.Forget(context.Background(), cached.ID, true); err != nil {
		t.Fatal(err)
	}
	replacement, err := c.Doctor(context.Background(), "worker")
	if err != nil || replacement.ID != newID || replacement.Launch != "starting" || replacement.Retired {
		t.Fatalf("replacement changed: %#v, %v", replacement, err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "jobs", newID, "initial-task.txt")); err != nil {
		t.Fatalf("replacement task was removed: %v", err)
	}
}

func TestForgetByIDRejectsNameWithoutRevokingJob(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "123e4567-e89b-42d3-a456-426614174000"
	if _, err := createLifecycleRecord(t, s, id, "worker", "preexec", nil, nil); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	err = c.Forget(context.Background(), "worker", true)
	var action *ActionError
	if !errors.As(err, &action) || action.ExitCode != 2 {
		t.Fatalf("by-ID name must be invalid usage, got %v", err)
	}
	_, launch, err := s.load(id)
	if err != nil || launch.Phase != "preexec" || launch.Retired {
		t.Fatalf("invalid ID changed job: %#v, %v", launch, err)
	}
}
