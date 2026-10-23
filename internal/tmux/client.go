package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (c Client) ValidateVersion(ctx context.Context) error {
	out, err := c.command(ctx, "-V")
	if err != nil {
		return fmt.Errorf("tmux version check failed")
	}
	version := strings.TrimSpace(strings.TrimPrefix(string(out), "tmux "))
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("unsupported tmux version")
	}
	major, err1 := strconv.Atoi(parts[0])
	minorText := strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' })
	minor, err2 := strconv.Atoi(minorText)
	if err1 != nil || err2 != nil || major < 3 || major == 3 && minor < 5 {
		return fmt.Errorf("tmux 3.5 or newer is required")
	}
	return nil
}

func (c Client) Create(ctx context.Context, o CreateOptions) error {
	if o.Socket == "" || o.Session == "" || o.Executable == "" || o.JobID == "" || o.RuntimeDir == "" || o.Width < 1 || o.Height < 1 || o.Config == "" {
		return fmt.Errorf("invalid tmux create options")
	}
	args := []string{"-S", o.Socket, "-f", o.Config, "new-session", "-d", "-s", o.Session, "-x", strconv.Itoa(o.Width), "-y", strconv.Itoa(o.Height), "-c", o.RuntimeDir, o.Executable, "_run", o.JobID}
	cmd := exec.CommandContext(ctx, c.executable(), args...)
	cmd.Env = o.Env
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux session creation failed")
	}
	return nil
}

func (c Client) Inspect(ctx context.Context, socket, session string) (Snapshot, error) {
	if socket == "" || session == "" {
		return Snapshot{}, fmt.Errorf("invalid tmux inspect target")
	}
	format := strings.Join([]string{"#{@ac_owner}", "#{@ac_job}", "#{@ac_protocol}", "#{session_name}", "#{pane_id}", "#{pid}", "#{pane_pid}", "#{pane_dead}", "#{pane_dead_status}", "#{session_attached}", "#{window_width}", "#{window_height}"}, "|")
	out, err := c.command(ctx, "-S", socket, "-N", "display-message", "-p", "-t", "="+session+":0.0", format)
	if err != nil {
		return Snapshot{}, fmt.Errorf("tmux session inspection failed")
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "|")
	if len(fields) != 12 {
		return Snapshot{}, fmt.Errorf("invalid tmux field count: %d", len(fields))
	}
	if fields[3] != session || fields[4] == "" {
		return Snapshot{}, fmt.Errorf("invalid tmux session or pane target")
	}
	serverPID, err := strconv.Atoi(fields[5])
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid tmux server PID")
	}
	panePID, err := strconv.Atoi(fields[6])
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid tmux pane PID")
	}
	clients, err := strconv.Atoi(fields[9])
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid tmux client count")
	}
	width, err := strconv.Atoi(fields[10])
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid tmux window width")
	}
	height, err := strconv.Atoi(fields[11])
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid tmux window height")
	}
	dead, err := strconv.ParseBool(fields[7])
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid tmux pane-dead flag")
	}
	if serverPID < 1 || panePID < 1 || clients < 0 || width < 1 || height < 1 {
		return Snapshot{}, fmt.Errorf("invalid tmux numeric bounds")
	}
	s := Snapshot{Owner: fields[0], Job: fields[1], Protocol: fields[2], Session: fields[3], PaneID: fields[4], ServerPID: serverPID, PanePID: panePID, PaneDead: dead, Clients: clients, Width: width, Height: height}
	if dead && fields[8] != "" {
		status, parseErr := strconv.Atoi(fields[8])
		if parseErr != nil {
			return Snapshot{}, fmt.Errorf("invalid tmux exit status")
		}
		s.ExitStatus = &status
	}
	return s, nil
}

func (c Client) AttachCommand(ctx context.Context, socket, session string, observe bool) *exec.Cmd {
	args := []string{"-S", socket, "-N", "attach-session", "-E"}
	if observe {
		args = append(args, "-r")
	}
	args = append(args, "-t", "="+session)
	cmd := exec.CommandContext(ctx, c.executable(), args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Second
	return cmd
}

func (c Client) Remove(ctx context.Context, socket, session string) error {
	if socket == "" || session == "" {
		return fmt.Errorf("invalid tmux remove target")
	}
	if _, err := c.command(ctx, "-S", socket, "-N", "kill-session", "-t", "="+session); err != nil {
		return fmt.Errorf("tmux session removal failed")
	}
	return nil
}

func (c Client) executable() string {
	if c.Executable == "" {
		return "tmux"
	}
	return c.Executable
}

func (c Client) command(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.executable(), args...)
	return cmd.Output()
}
