package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"agent-coordinator/internal/tmux"
	"golang.org/x/sys/unix"
)

func (c *Coordinator) ownedSnapshot(ctx context.Context, m Manifest) (tmux.Snapshot, error) {
	var empty tmux.Snapshot
	expected, err := c.store.socket(m.ID, false)
	if err != nil || expected != m.Socket || m.Owner != c.store.token || m.Protocol != ProtocolVersion {
		return empty, errors.New("invalid resource identity")
	}
	for _, dir := range []string{filepath.Dir(filepath.Dir(m.Socket)), filepath.Dir(m.Socket)} {
		var st unix.Stat_t
		if err := unix.Lstat(dir, &st); err != nil {
			return empty, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0777 != 0700 || st.Uid != uint32(os.Getuid()) {
			return empty, errors.New("invalid socket directory ownership")
		}
	}
	var st unix.Stat_t
	if err := unix.Lstat(m.Socket, &st); err != nil {
		return empty, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != uint32(os.Getuid()) {
		return empty, errors.New("invalid socket ownership")
	}
	snap, err := (tmux.Client{Executable: m.Tmux}).Inspect(ctx, m.Socket, "ac-"+m.ID)
	if err != nil {
		return empty, err
	}
	if snap.Owner != m.Owner || snap.Job != m.ID || snap.Protocol != m.Protocol || snap.Session != "ac-"+m.ID || snap.ServerPID <= 0 || snap.PanePID <= 0 || snap.PaneID == "" {
		return empty, errors.New("tmux ownership markers disagree")
	}
	return snap, nil
}

func sameIdentity(a, b ProcessIdentity) bool {
	return a.BootID == b.BootID && a.PID == b.PID && a.UID == b.UID && a.StartSec == b.StartSec && a.StartUsec == b.StartUsec
}
