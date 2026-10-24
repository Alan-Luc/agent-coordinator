package coordinator

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"agent-coordinator/internal/tmux"
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
	if validateName(name) != nil || validateTask(task) != nil {
		return JobFacts{}, actionError(2, "invalid_input", "invalid name or task: nonblank UTF-8 task <=1 MiB required", false, "", name)
	}
	directory, err := canonicalCwd(cwd)
	if err != nil {
		return JobFacts{}, actionError(2, "invalid_cwd", "cwd must be an existing directory", false, "", name)
	}
	pi, err := executable(c.options.PiPath, "pi")
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
	id, err := newID()
	if err != nil {
		return JobFacts{}, err
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
	if err := writeBytes(staging, "initial-task.txt", append([]byte(TaskHeader), task...)); err != nil {
		return JobFacts{}, err
	}
	if err := writeBytes(staging, "tmux.conf", []byte(tmux.Config(c.store.token, id, name))); err != nil {
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
	f.Close()
	if err := syncDir(staging); err != nil {
		return JobFacts{}, err
	}
	job := filepath.Join(c.store.root, "jobs", id)
	if err := os.Rename(staging, job); err != nil {
		return JobFacts{}, err
	}
	if err := syncDir(filepath.Dir(job)); err != nil {
		return JobFacts{}, actionError(75, "uncertain_publication", "job publication uncertain; inspect list/doctor, do not retry", true, id, name)
	}
	unlock()
	locked = false
	width, height := c.options.Width, c.options.Height
	if width <= 0 || height <= 0 {
		width, height = 120, 30
	}
	launchCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	// Submission happens once. A failed client may still have reached the server.
	_ = client.Create(launchCtx, tmux.CreateOptions{
		Socket:     socket,
		Config:     filepath.Join(job, "tmux.conf"),
		Session:    "ac-" + id,
		Executable: self,
		JobID:      id,
		RuntimeDir: job,
		Env:        launchEnvironment(c.store.root, c.store.token, id),
		Width:      width,
		Height:     height,
	})
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
