//go:build darwin

package coordinator

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func requirePeerPID(conn net.Conn, expected int) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok || expected <= 0 {
		return errors.New("invalid Unix peer")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var peer int
	var socketErr error
	if err := raw.Control(func(fd uintptr) { peer, socketErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) }); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if peer != expected {
		return errors.New("Unix peer PID mismatch")
	}
	return nil
}

func syscallKillGroup(pid int) error { return unix.Kill(-pid, unix.SIGKILL) }
