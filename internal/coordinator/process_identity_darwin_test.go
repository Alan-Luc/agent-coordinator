//go:build darwin

package coordinator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestProcessIdentityOwnedProcesses(t *testing.T) {
	current, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if current.PID != os.Getpid() || current.UID != uint32(os.Getuid()) || current.BootID == "" || current.StartSec <= 0 {
		t.Fatalf("invalid current identity: %+v", current)
	}
	status, err := compareProcess(current)
	if err != nil || status != ProcessAlive {
		t.Fatalf("current process: status=%q err=%v", status, err)
	}

	child := exec.Command("/bin/sleep", "1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(child.Process.Pid)
	if err != nil {
		_ = child.Wait()
		t.Fatal(err)
	}
	if identity.PID != child.Process.Pid {
		t.Fatalf("child identity PID = %d", identity.PID)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	status, err = compareProcess(identity)
	if err != nil || status != ProcessEnded {
		t.Fatalf("exited child: status=%q err=%v", status, err)
	}
}

func TestCompareProcessMismatches(t *testing.T) {
	current, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ProcessIdentity)
	}{
		{"boot", func(p *ProcessIdentity) { p.BootID += "-other" }},
		{"birth", func(p *ProcessIdentity) { p.StartUsec++ }},
		{"uid", func(p *ProcessIdentity) { p.UID++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorded := current
			tc.mutate(&recorded)
			status, err := compareProcess(recorded)
			if err != nil || status != ProcessEnded {
				t.Fatalf("status=%q err=%v", status, err)
			}
		})
	}
}

func TestIdentifyProcessInvalidPIDIsGone(t *testing.T) {
	for _, pid := range []int{0, -1, int(^uint(0) >> 1)} {
		_, err := identifyProcess(pid)
		if !errors.Is(err, ErrProcessGone) {
			t.Errorf("identifyProcess(%d) error = %v, want ErrProcessGone", pid, err)
		}
	}
}

func TestProcessIdentitySurvivesExec(t *testing.T) {
	if release := os.Getenv("AC_TEST_EXEC_RELEASE"); release != "" {
		for {
			if _, err := os.Stat(release); err == nil {
				if err := unix.Exec("/bin/sleep", []string{"sleep", "1"}, os.Environ()); err != nil {
					os.Exit(91)
				}
			}
			time.Sleep(time.Millisecond)
		}
	}
	path := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessIdentitySurvivesExec$")
	child.Env = append(os.Environ(), "AC_TEST_EXEC_RELEASE="+path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Wait()
	before, err := identifyProcess(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", child.Process.Pid)
		if err != nil || len(processes) != 1 {
			t.Fatalf("read owned exec probe: %v", err)
		}
		if unix.ByteSliceToString(processes[0].Proc.P_comm[:]) == "sleep" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("owned helper did not exec sleep")
		case <-time.After(time.Millisecond):
		}
	}
	after, err := identifyProcess(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if !sameIdentity(before, after) {
		t.Fatalf("exec changed birth identity: before=%+v after=%+v", before, after)
	}
}

func TestProcessGroupOwnedProcess(t *testing.T) {
	pgid, err := processGroup(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if pgid <= 0 {
		t.Fatalf("process group = %d", pgid)
	}
}
