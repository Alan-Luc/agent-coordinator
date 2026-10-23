package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTaskAndNameValidation(t *testing.T) {
	for _, name := range []string{"a", "restaurant-a", "0"} {
		if err := validateName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "A", "../x", "-a", "a;b", string(make([]byte, 49))} {
		if validateName(name) == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	for _, task := range [][]byte{[]byte("\xef\xbb\xbfhello\n世界\n@ / - $(touch malicious) </file>"), []byte("task")} {
		if err := validateTask(task); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range [][]byte{nil, []byte(" \n\t"), {0xff}, make([]byte, MaxTaskBytes+1)} {
		if validateTask(task) == nil {
			t.Error("accepted invalid task")
		}
	}
}

func TestPrivateStoreRejectsManagedSymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("root is not private: %v %v", info, err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte(`{"schema":1,"token":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "owner.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "owner.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(root); err == nil {
		t.Fatal("followed managed owner symlink")
	}
	_ = s
}

func TestLaunchLockRejectsSymlinkedJobDirectory(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174000"
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.root, "jobs", id)); err != nil {
		t.Fatal(err)
	}
	release, err := s.lock(context.Background(), filepath.Join("jobs", id, "launch.lock"))
	if err == nil {
		release()
		t.Error("accepted lock under symlinked job directory")
	}
	if _, err := os.Lstat(filepath.Join(outside, "launch.lock")); !os.IsNotExist(err) {
		t.Fatal("lock creation escaped managed state")
	}
}

func TestManagedFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		file, err := privateFile(path, unix.O_RDONLY)
		if file != nil {
			_ = file.Close()
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("accepted FIFO as managed regular file")
		}
	case <-time.After(time.Second):
		writer, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(writer)
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("blocked FIFO reader did not exit after owned writer opened")
		}
		t.Fatal("managed FIFO blocked before the file-type check")
	}
}

func TestOpenPreservesRegistryLockTimeoutExitCode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.lock(context.Background(), "registry.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = Open(Options{StateDir: root})
	var action *ActionError
	if !errors.As(err, &action) || action.ExitCode != 3 {
		t.Fatalf("lock timeout should preserve exit code 3, got %v", err)
	}
}

func TestRegistryLockTimesOutWithoutReplacingInode(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock(context.Background(), "registry.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if release, err := s.lock(ctx, "registry.lock"); err == nil {
		release()
		t.Fatal("second lock acquired")
	}
}
