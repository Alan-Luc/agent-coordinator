//go:build darwin

package coordinator

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRequirePeerPIDMatchesExactUnixClient(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ac-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := requirePeerPID(server, os.Getpid()); err != nil {
		t.Fatalf("exact client PID rejected: %v", err)
	}
	if err := requirePeerPID(server, os.Getpid()+1); err == nil {
		t.Fatal("wrong client PID accepted")
	}
}
