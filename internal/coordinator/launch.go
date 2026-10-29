package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"agent-coordinator/internal/pilaunch"
	"agent-coordinator/internal/tmux"
	"golang.org/x/sys/unix"
)

type Coordinator struct {
	store   *store
	options Options
}

func Open(options Options) (*Coordinator, error) {
	if runtime.GOOS != "darwin" {
		return nil, actionError(2, "unsupported_platform", "agent-coordinator v1 supports macOS only", false, "", "")
	}
	s, err := openStore(options.StateDir)
	if err != nil {
		var action *ActionError
		if errors.As(err, &action) {
			return nil, action
		}
		return nil, actionError(1, "state_unavailable", "private coordinator state is unavailable", false, "", "")
	}
	return &Coordinator{store: s, options: options}, nil
}

func actionError(exit int, code, message string, uncertain bool, id, name string) error {
	if !idPattern.MatchString(id) {
		id = ""
	}
	if validateName(name) != nil {
		name = ""
	}
	return &ActionError{
		ExitCode:   exit,
		Diagnostic: Diagnostic{Code: code, Message: message, Uncertain: uncertain, ID: id, Name: name},
	}
}

func executable(path, name string) (string, error) {
	if path == "" {
		var err error
		path, err = exec.LookPath(name)
		if err != nil {
			return "", errors.New("required executable unavailable")
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("required executable unavailable")
	}
	return abs, nil
}
func canonicalCwd(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("cwd must be an existing directory")
	}
	return abs, nil
}
func lockStagedLaunch(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockStagedLaunch(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func interactiveLaunchEnvironment(root, owner, id string) []string {
	return interactiveLaunchEnvironmentFrom(os.Environ(), root, owner, id)
}

func interactiveLaunchEnvironmentFrom(base []string, root, owner, id string) []string {
	env := []string{}
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AC_") {
			continue
		}
		switch key {
		case "PI_SESSION_ID", "PI_SESSION_FILE", "KITTY_WINDOW_ID", "KITTY_LISTEN_ON", "KITTY_PID", "WINDOWID":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "AC_STATE_DIR="+root, "AC_OWNER="+owner, "AC_JOB="+id)
}

func launchEnvironment(root, owner, id string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AC_") {
			continue
		}
		switch key {
		case "PI_SESSION_ID", "PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL", "PI_REASONING_LEVEL":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "AC_STATE_DIR="+root, "AC_OWNER="+owner, "AC_JOB="+id)
}
func (c *Coordinator) Start(ctx context.Context, name, cwd string, task []byte) (JobFacts, error) {
	return c.start(ctx, name, cwd, task, nil)
}

func (c *Coordinator) startManaged(ctx context.Context, name, cwd string, task []byte, expected CommunicationManager) (JobFacts, error) {
	if !idPattern.MatchString(expected.ID) || expected.Lease == nil {
		return JobFacts{}, actionError(2, "invalid_manager", "managed start requires an enrolled manager", false, "", name)
	}
	return c.start(ctx, name, cwd, task, &expected)
}

func (c *Coordinator) start(ctx context.Context, name, cwd string, task []byte, expected *CommunicationManager) (JobFacts, error) {
	return c.startTransaction(ctx, name, cwd, task, nil, expected)
}

func (c *Coordinator) startInteractive(ctx context.Context, cwd string, argv []string, native pilaunch.Managed, payload []byte) (JobFacts, error) {
	id, err := newID()
	if err != nil {
		return JobFacts{}, err
	}
	name := "pi-" + id[:8]
	return c.startTransaction(ctx, name, cwd, nil, &interactiveStart{ID: id, Argv: argv, Native: native, Payload: payload}, nil)
}

type interactiveStart struct {
	ID      string
	Argv    []string
	Native  pilaunch.Managed
	Payload []byte
}

func (c *Coordinator) startTransaction(ctx context.Context, name, cwd string, task []byte, interactive *interactiveStart, expected *CommunicationManager) (JobFacts, error) {
	if validateName(name) != nil || interactive == nil && validateTask(task) != nil {
		return JobFacts{}, actionError(2, "invalid_input", "invalid name or task: nonblank UTF-8 task <=1 MiB required", false, "", name)
	}
	directory, err := canonicalCwd(cwd)
	if err != nil {
		return JobFacts{}, actionError(2, "invalid_cwd", "cwd must be an existing directory", false, "", name)
	}
	pi, err := executable(c.options.PiPath, "pi")
	if interactive != nil {
		pi = interactive.Native.Executable
		err = pilaunch.Recheck(interactive.Native)
	}
	if err != nil {
		return JobFacts{}, actionError(1, "missing_pi", "Pi executable unavailable on PATH", false, "", name)
	}
	tmuxPath, err := executable(c.options.TmuxPath, "tmux")
	if err != nil {
		return JobFacts{}, actionError(1, "missing_tmux", "tmux >=3.5 required on PATH", false, "", name)
	}
	self := c.options.Executable
	if self == "" {
		self, err = os.Executable()
		if err != nil {
			return JobFacts{}, err
		}
	}
	self, err = executable(self, "")
	if err != nil || !regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`).MatchString(self) {
		return JobFacts{}, actionError(2, "unsafe_executable", "coordinator executable path is not safe for tmux command parsing", false, "", name)
	}
	client := tmux.Client{Executable: tmuxPath}
	if err := client.ValidateVersion(ctx); err != nil {
		return JobFacts{}, actionError(1, "unsupported_tmux", "tmux >=3.5 required", false, "", name)
	}
	boot, err := currentBoot()
	if err != nil {
		return JobFacts{}, actionError(1, "identity_unavailable", "macOS kernel identity is unavailable", false, "", name)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unlock, err := c.store.lock(lockCtx, "registry.lock")
	if err != nil {
		return JobFacts{}, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	ids, err := c.store.ids()
	if err != nil {
		return JobFacts{}, err
	}
	for _, id := range ids {
		m, l, loadErr := c.store.load(id)
		if m.Name == name && (!l.Retired || loadErr != nil || hasLegacyProcessEvidence(m, l)) {
			return JobFacts{}, actionError(3, "duplicate_name", "name is already reserved; inspect list or doctor", false, id, name)
		}
	}
	var communication communicationState
	if expected != nil {
		if expected.Lease == nil {
			return JobFacts{}, actionError(2, "invalid_manager", "managed start requires an active manager lease", false, "", name)
		}
		var stateErr error
		communication, stateErr = c.communicationLoad()
		if stateErr != nil {
			return JobFacts{}, stateErr
		}
		currentParent, parentErr := identifyProcess(expected.Lease.Process.PID)
		parentMatches := parentErr == nil && sameIdentity(currentParent, expected.Lease.Process)
		managerFound := false
		for _, manager := range communication.Managers {
			if parentMatches && manager.ID == expected.ID && manager.SessionFile == expected.SessionFile && manager.SessionID == expected.SessionID && manager.Enabled && manager.Lease != nil && manager.Lease.Generation == expected.Lease.Generation && sameIdentity(manager.Lease.Process, expected.Lease.Process) {
				managerFound = true
			}
		}
		if !managerFound {
			return JobFacts{}, actionError(1, "manager_unavailable", "manager is not enabled", false, "", name)
		}
		if len(communication.Bindings) >= 128 {
			communication.Degraded = true
			if err := c.communicationSave(communication); err != nil {
				return JobFacts{}, err
			}
			return JobFacts{}, actionError(1, "queue_full", "binding capacity is full; job was not dispatched", false, "", name)
		}
	}
	id := ""
	if interactive != nil {
		id = interactive.ID
	} else {
		id, err = newID()
		if err != nil {
			return JobFacts{}, err
		}
	}
	socket, err := c.store.socket(id, true)
	if err != nil {
		return JobFacts{}, err
	}
	m := Manifest{
		Schema:       SchemaVersion,
		ID:           id,
		Name:         name,
		Owner:        c.store.token,
		Cwd:          directory,
		Pi:           pi,
		Tmux:         tmuxPath,
		Coordinator:  self,
		Created:      time.Now().UTC(),
		BootID:       boot,
		BootIDScheme: BootIdentityScheme,
		Protocol:     ProtocolVersion,
		Socket:       socket,
	}
	if interactive != nil {
		m.Protocol = InteractiveProtocolVersion
	}
	staging, err := os.MkdirTemp(filepath.Join(c.store.root, "jobs"), ".staging-")
	if err != nil {
		return JobFacts{}, err
	}
	defer os.RemoveAll(staging)
	for file, value := range map[string]interface{}{"manifest.json": m, "launch.json": Launch{Schema: SchemaVersion, Phase: "preexec"}} {
		if err := writeJSON(staging, file, value); err != nil {
			return JobFacts{}, err
		}
	}
	if interactive == nil {
		if err := writeBytes(staging, "initial-task.txt", append([]byte(TaskHeader), task...)); err != nil {
			return JobFacts{}, err
		}
	} else {
		sum := sha256.Sum256(interactive.Payload)
		intent := InteractiveIntent{Schema: SchemaVersion, Kind: "interactive", TerminalProfile: TransparentTerminalProfile, ArgvSHA256: hex.EncodeToString(sum[:]), NativePi: &interactive.Native}
		if err := writeJSON(staging, "intent.json", intent); err != nil {
			return JobFacts{}, err
		}
		if err := writeJSON(staging, "role.json", JobRole{Schema: SchemaVersion, Role: "standalone"}); err != nil {
			return JobFacts{}, err
		}
		if err := writeBytes(staging, "launch-argv.json", interactive.Payload); err != nil {
			return JobFacts{}, err
		}
	}
	config := tmux.Config(c.store.token, id, name)
	if interactive != nil {
		config = tmux.TransparentConfig(c.store.token, id)
	}
	if err := writeBytes(staging, "tmux.conf", []byte(config)); err != nil {
		return JobFacts{}, err
	}
	f, err := privateFile(filepath.Join(staging, "launch.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return JobFacts{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return JobFacts{}, err
	}
	launchLocked := false
	if interactive != nil {
		if err := lockStagedLaunch(f); err != nil {
			f.Close()
			return JobFacts{}, err
		}
		launchLocked = true
	}
	launchFileOpen := true
	unlockLaunch := func() {
		if launchFileOpen {
			if launchLocked {
				unlockStagedLaunch(f)
				launchLocked = false
			}
			_ = f.Close()
			launchFileOpen = false
		}
	}
	defer unlockLaunch()
	if err := syncDir(staging); err != nil {
		unlockLaunch()
		return JobFacts{}, err
	}
	job := filepath.Join(c.store.root, "jobs", id)
	if err := os.Rename(staging, job); err != nil {
		unlockLaunch()
		return JobFacts{}, err
	}
	if err := syncDir(filepath.Dir(job)); err != nil {
		if interactive != nil {
			_ = writeJSON(job, "launch.json", Launch{Schema: SchemaVersion, Phase: "revoked", Retired: true, Error: "publication sync failed"})
		}
		unlockLaunch()
		return JobFacts{}, actionError(75, "uncertain_publication", "job publication uncertain; inspect list/doctor, do not retry", true, id, name)
	}
	if expected != nil {
		communication.Bindings = append(communication.Bindings, CommunicationBinding{JobID: id, ManagerID: expected.ID})
		if err := c.communicationSave(communication); err != nil {
			return JobFacts{}, actionError(75, "binding_uncertain", "job binding could not be durably saved; do not launch", true, id, name)
		}
	}
	unlock()
	locked = false
	width, height := c.options.Width, c.options.Height
	if width <= 0 || height <= 0 {
		width, height = 120, 30
	}
	launchCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if interactive != nil {
		topologyCtx, cancelTopology := context.WithTimeout(launchCtx, 5*time.Second)
		_ = client.Create(topologyCtx, tmux.CreateOptions{Socket: socket, Config: filepath.Join(job, "tmux.conf"), Session: "ac-" + id, Executable: self, JobID: id, RuntimeDir: job, Env: interactiveLaunchEnvironment(c.store.root, c.store.token, id), Width: width, Height: height})
		if topologyCtx.Err() == nil {
			_ = client.CreateReturnView(topologyCtx, socket, "ac-"+id, "ac-return-"+id)
		}
		if topologyCtx.Err() == nil {
			_, err = client.InspectTransparent(topologyCtx, socket, "ac-"+id, "ac-return-"+id)
		} else {
			err = topologyCtx.Err()
		}
		cancelTopology()
		if err != nil {
			l := Launch{Schema: SchemaVersion, Phase: "revoked", Retired: true, Error: "interactive topology creation failed"}
			_ = writeJSON(job, "launch.json", l)
			unlockLaunch()
			return JobFacts{ID: id, Name: name, Launch: "revoked", Terminal: "unknown", PiReadiness: "unverified", TaskOutcome: "unknown", NextAction: "inspect by ID; do not retry"}, actionError(75, "topology_unconfirmed", "interactive topology is incomplete; inspect by ID, do not retry", true, id, name)
		}
		unlockLaunch()
	} else {
		// Submission happens once. A failed client may still have reached the server.
		_ = client.Create(launchCtx, tmux.CreateOptions{Socket: socket, Config: filepath.Join(job, "tmux.conf"), Session: "ac-" + id, Executable: self, JobID: id, RuntimeDir: job, Env: launchEnvironment(c.store.root, c.store.token, id), Width: width, Height: height})
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, launch, loadErr := c.store.load(id)
		if loadErr == nil {
			if launch.Phase == "failed" || launch.Phase == "revoked" {
				return c.facts(ctx, current, launch), actionError(1, "launch_failed", "launch did not complete; inspect doctor", false, id, name)
			}
			if launch.Phase == "exec-intent" {
				facts := c.facts(ctx, current, launch)
				if facts.Launch == "submitted" || facts.Launch == "exited" {
					facts.Launch = "submitted"
					return facts, nil
				}
			}
		}
		select {
		case <-launchCtx.Done():
			return JobFacts{ID: id, Name: name, Launch: "unconfirmed", Terminal: "unknown", PiReadiness: "unverified", TaskOutcome: "unknown", NextAction: "inspect list/doctor; do not retry"}, actionError(75, "uncertain_launch", "launch uncertain; inspect list/doctor before any new attempt", true, id, name)
		case <-ticker.C:
		}
	}
}

func ReadTask(reader io.Reader) ([]byte, error) {
	task, err := io.ReadAll(io.LimitReader(reader, MaxTaskBytes+1))
	if err != nil {
		return nil, errors.New("cannot read task")
	}
	if err := validateTask(task); err != nil {
		return nil, err
	}
	return task, nil
}
