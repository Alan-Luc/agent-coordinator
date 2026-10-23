//go:build darwin

package coordinator

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

func currentBoot() (string, error) {
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", fmt.Errorf("query kernel boot time: %w", err)
	}
	return strconv.FormatInt(boot.Sec, 10) + "." + strconv.FormatInt(int64(boot.Usec), 10), nil
}

func identifyProcess(pid int) (ProcessIdentity, error) {
	if pid <= 0 || int64(pid) > int64(^uint32(0)>>1) {
		return ProcessIdentity{}, ErrProcessGone
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("query process %d: %w", pid, err)
	}
	if len(processes) == 0 {
		return ProcessIdentity{}, ErrProcessGone
	}
	if len(processes) != 1 || int(processes[0].Proc.P_pid) != pid {
		return ProcessIdentity{}, fmt.Errorf("query process %d: unexpected kernel process record", pid)
	}
	process := &processes[0]
	bootID, err := currentBoot()
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{
		BootID:    bootID,
		PID:       pid,
		UID:       process.Eproc.Ucred.Uid,
		StartSec:  process.Proc.P_starttime.Sec,
		StartUsec: int64(process.Proc.P_starttime.Usec),
	}, nil
}

func compareProcess(recorded ProcessIdentity) (ProcessStatus, error) {
	bootID, err := currentBoot()
	if err != nil {
		return ProcessUnknown, err
	}
	if recorded.BootID != bootID {
		return ProcessEnded, nil
	}
	current, err := identifyProcess(recorded.PID)
	if errors.Is(err, ErrProcessGone) {
		return ProcessEnded, nil
	}
	if err != nil {
		return ProcessUnknown, err
	}
	if current.BootID != recorded.BootID || current.StartSec != recorded.StartSec || current.StartUsec != recorded.StartUsec || current.UID != recorded.UID {
		return ProcessEnded, nil
	}
	return ProcessAlive, nil
}

func foregroundProcessGroup(pid int) (int, error) {
	if pid <= 0 {
		return 0, ErrProcessGone
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("query foreground process group: %w", err)
	}
	if len(processes) == 0 {
		return 0, ErrProcessGone
	}
	if len(processes) != 1 || int(processes[0].Proc.P_pid) != pid {
		return 0, errors.New("unexpected kernel process record")
	}
	return int(processes[0].Eproc.Tpgid), nil
}

func processGroup(pid int) (int, error) {
	if pid <= 0 {
		return 0, ErrProcessGone
	}
	group, err := unix.Getpgid(pid)
	if errors.Is(err, unix.ESRCH) {
		return 0, ErrProcessGone
	}
	if err != nil {
		return 0, fmt.Errorf("query process group for %d: %w", pid, err)
	}
	return group, nil
}
