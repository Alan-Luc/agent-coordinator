//go:build !darwin

package coordinator

import (
	"errors"
	"net"
)

func requirePeerPID(net.Conn, int) error {
	return errors.New("Unix peer PID verification is unsupported")
}
func syscallKillGroup(int) error { return errors.New("process signaling is unsupported") }
