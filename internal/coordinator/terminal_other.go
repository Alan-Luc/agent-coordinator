//go:build !darwin

package coordinator

import (
	"context"
	"errors"
	"os/exec"
)

func TerminalSize(fd int) (int, int, error) {
	return 0, 0, errors.New("terminal actions are supported only on macOS")
}

func RunTerminal(ctx context.Context, cmd *exec.Cmd) error {
	return errors.New("terminal actions are supported only on macOS")
}
