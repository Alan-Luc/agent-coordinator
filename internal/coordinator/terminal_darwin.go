//go:build darwin

package coordinator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func TerminalSize(fd int) (int, int, error) {
	if _, err := unix.IoctlGetTermios(fd, unix.TIOCGETA); err != nil {
		return 0, 0, err
	}
	size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	if size.Col == 0 || size.Row == 0 {
		return 0, 0, errors.New("terminal geometry unavailable")
	}
	return int(size.Col), int(size.Row), nil
}

func RunTerminal(ctx context.Context, cmd *exec.Cmd) (result error) {
	saved, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TIOCGETA)
	if err != nil {
		return err
	}
	if _, err := unix.IoctlGetTermios(int(os.Stdout.Fd()), unix.TIOCGETA); err != nil {
		return err
	}
	defer func() {
		restoreErr := unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, saved)
		_, screenErr := os.Stdout.WriteString("\x1b[?1049l\x1b[?25h\x1b[0m")
		result = errors.Join(result, restoreErr, screenErr)
	}()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// This is our attached client child, never the tmux server or Pi process.
		signalErr := cmd.Process.Signal(syscall.SIGTERM)
		waitErr := <-done
		return errors.Join(ctx.Err(), signalErr, waitErr)
	}
}
