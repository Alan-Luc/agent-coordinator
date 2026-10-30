package coordinator

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coordinate/internal/pilaunch"
	"golang.org/x/sys/unix"
)

// RunBootstrap is the fenced pane entry point, not a public launch interface.
func RunBootstrap(ctx context.Context, id string) error {
	if !idPattern.MatchString(id) || os.Getenv("AC_JOB") != id || os.Getenv("AC_STATE_DIR") == "" || !tokenPattern.MatchString(os.Getenv("AC_OWNER")) {
		return errors.New("invalid bootstrap identity")
	}
	c, err := Open(Options{StateDir: os.Getenv("AC_STATE_DIR")})
	if err != nil {
		return err
	}
	if c.store.token != os.Getenv("AC_OWNER") {
		return errors.New("bootstrap installation mismatch")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	release, err := c.store.lock(lockCtx, filepath.Join("jobs", id, "launch.lock"))
	if err != nil {
		return err
	}
	locked := true
	defer func() {
		if locked {
			release()
		}
	}()
	m, l, err := c.store.load(id)
	if err != nil {
		return errors.New("invalid bootstrap records")
	}
	if !supportedJobProtocol(m.Protocol) {
		return errors.New("unsupported bootstrap protocol")
	}
	if l.Phase != "preexec" || l.Retired {
		return errors.New("launch attempt already claimed or revoked")
	}
	if m.BootIDScheme != BootIdentityScheme || !bootUUIDPattern.MatchString(m.BootID) {
		return errors.New("legacy or unsupported boot identity cannot authorize bootstrap")
	}
	fail := func() error {
		l.Phase = "failed"
		l.Error = "bootstrap validation failed"
		if err := writeJSON(filepath.Join(c.store.root, "jobs", id), "launch.json", l); err != nil {
			return errors.New("cannot persist bootstrap validation failure")
		}
		return errors.New("bootstrap validation failed")
	}
	boot, err := currentBoot()
	if err != nil || boot != m.BootID {
		return fail()
	}
	cwd, err := canonicalCwd(m.Cwd)
	if err != nil || cwd != m.Cwd {
		return fail()
	}
	pi, err := executable(m.Pi, "")
	if err != nil || pi != m.Pi {
		return fail()
	}
	job := filepath.Join(c.store.root, "jobs", id)
	var nativeArgv []string
	consumeInteractivePayload := false
	if m.Protocol == InteractiveProtocolVersion {
		intent, intentErr := readInteractiveIntent(job)
		if intentErr != nil || intent.NativePi.Executable != m.Pi || pilaunch.Recheck(*intent.NativePi) != nil {
			return fail()
		}
		nativeArgv, err = readInteractiveArguments(job)
		if err != nil {
			return fail()
		}
		consumeInteractivePayload = true
	} else {
		taskPath := filepath.Join(job, "initial-task.txt")
		task, taskErr := readPrivateBytes(taskPath, MaxTaskBytes+len(TaskHeader))
		if taskErr != nil || !strings.HasPrefix(string(task), TaskHeader) || validateTask(task[len(TaskHeader):]) != nil {
			return fail()
		}
	}
	snap, err := c.ownedSnapshot(ctx, m)
	if err != nil || snap.PanePID != os.Getpid() || snap.PaneID != os.Getenv("TMUX_PANE") || snap.PaneDead {
		return fail()
	}
	server, err := identifyProcess(snap.ServerPID)
	if err != nil || server.UID != uint32(os.Getuid()) {
		return fail()
	}
	pane, err := identifyProcess(os.Getpid())
	if err != nil || pane.UID != uint32(os.Getuid()) || server.BootID != boot || pane.BootID != boot {
		return fail()
	}
	if consumeInteractivePayload {
		if err := os.Remove(filepath.Join(job, "launch-argv.json")); err != nil || syncDir(job) != nil {
			return fail()
		}
	}
	l = Launch{Schema: SchemaVersion, Phase: "exec-intent", Server: &server, Pane: &pane}
	if err := writeJSON(job, "launch.json", l); err != nil {
		return errors.New("cannot persist exec intent")
	}
	release()
	locked = false
	if err = os.Chdir(m.Cwd); err == nil {
		if m.Protocol == InteractiveProtocolVersion {
			nativeArgv = append([]string{m.Pi}, nativeArgv...)
			err = unix.Exec(m.Pi, nativeArgv, interactiveLaunchEnvironmentFrom(os.Environ(), c.store.root, c.store.token, id))
		} else {
			err = unix.Exec(m.Pi, []string{m.Pi, "--session-id", "ac-" + id, "--name", m.Name, "--", "@" + filepath.Join(job, "initial-task.txt")}, os.Environ())
		}
	}
	// An exec failure is definite only for the bootstrap that recorded this intent.
	release, lockErr := c.store.lock(lockCtx, filepath.Join("jobs", id, "launch.lock"))
	if lockErr != nil {
		return errors.New("exec failed; cannot persist failure")
	}
	defer release()
	_, current, loadErr := c.store.load(id)
	if loadErr != nil || current.Phase != "exec-intent" || current.Pane == nil || !sameIdentity(*current.Pane, pane) {
		return errors.New("exec failed; launch evidence changed")
	}
	current.Phase = "failed"
	current.Error = "Pi exec failed"
	if writeJSON(job, "launch.json", current) != nil {
		return errors.New("exec failed; cannot persist failure")
	}
	return errors.New("Pi exec failed")
}

func readPrivateBytes(path string, limit int) ([]byte, error) {
	f, err := privateFile(path, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, errors.New("private input too large")
	}
	return b, nil
}
