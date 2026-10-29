package coordinator

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"agent-coordinator/internal/tmux"
	"golang.org/x/sys/unix"
)

func (c *Coordinator) Continue(ctx context.Context, name string) error {
	return c.continueJob(ctx, name, false)
}

func (c *Coordinator) ContinueID(ctx context.Context, id string) error {
	return c.continueJob(ctx, id, true)
}

func (c *Coordinator) continueJob(ctx context.Context, name string, byID bool) error {
	facts, err := c.doctor(ctx, name, byID)
	if err != nil {
		return err
	}
	m, l, err := c.store.load(facts.ID)
	if err != nil {
		return actionError(1, "ownership_unknown", "job ownership could not be verified", true, facts.ID, name)
	}
	s, err := c.ownedSnapshot(ctx, m)
	if err != nil {
		return actionError(1, "ownership_unknown", "owned tmux session could not be verified", true, m.ID, name)
	}
	if l.Server == nil || l.Pane == nil {
		return actionError(1, "identity_unknown", "recorded server and pane identities are required", true, m.ID, name)
	}
	server, serverErr := identifyProcess(s.ServerPID)
	pane, paneErr := identifyProcess(s.PanePID)
	if serverErr != nil || paneErr != nil || !sameIdentity(server, *l.Server) || !sameIdentity(pane, *l.Pane) || s.PaneDead {
		return actionError(1, "identity_changed", "foreground process identity changed", true, m.ID, name)
	}
	pgid, pgidErr := processGroup(pane.PID)
	foreground, foregroundErr := foregroundProcessGroup(pane.PID)
	if pgidErr != nil || foregroundErr != nil || !continueIdentityMatches(*l.Server, *l.Pane, server, pane, pgid, foreground) {
		return actionError(1, "unsafe_process_group", "foreground group is not the verified pane process group", true, m.ID, name)
	}
	freshSnapshot, err := c.ownedSnapshot(ctx, m)
	if err != nil || freshSnapshot.ServerPID != l.Server.PID || freshSnapshot.PanePID != l.Pane.PID || freshSnapshot.PaneDead {
		return actionError(1, "identity_changed", "owned foreground changed before Continue", true, m.ID, name)
	}
	freshServer, serverErr := identifyProcess(freshSnapshot.ServerPID)
	freshPane, paneErr := identifyProcess(freshSnapshot.PanePID)
	if serverErr != nil || paneErr != nil || !continueIdentityMatches(*l.Server, *l.Pane, freshServer, freshPane, pgid, foreground) {
		return actionError(1, "identity_changed", "foreground process identity changed before Continue", true, m.ID, name)
	}
	if err := syscall.Kill(-freshPane.PID, syscall.SIGCONT); err != nil {
		return actionError(1, "signal_failed", "Continue signal was not delivered", false, m.ID, name)
	}
	return nil
}

func continueIdentityMatches(recordedServer, recordedPane, server, pane ProcessIdentity, paneGroup, foregroundGroup int) bool {
	return sameIdentity(recordedServer, server) && sameIdentity(recordedPane, pane) && paneGroup == pane.PID && foregroundGroup == pane.PID
}

func (c *Coordinator) RepairSocket(ctx context.Context, name string) error {
	return c.repairSocket(ctx, name, false)
}

func (c *Coordinator) RepairSocketID(ctx context.Context, id string) error {
	return c.repairSocket(ctx, id, true)
}

func (c *Coordinator) repairSocket(ctx context.Context, name string, byID bool) error {
	facts, err := c.doctor(ctx, name, byID)
	if err != nil {
		return err
	}
	if facts.Launch != "unreachable" {
		return actionError(1, "not_repairable", "only a verified live server with a lost socket can be repaired", true, facts.ID, name)
	}
	m, l, err := c.store.load(facts.ID)
	if err != nil || l.Server == nil {
		return actionError(1, "ownership_unknown", "recorded server identity is unavailable", true, facts.ID, name)
	}
	status, err := compareProcess(*l.Server)
	if err != nil || status != ProcessAlive {
		return actionError(1, "identity_changed", "recorded server is not verifiably live", true, m.ID, name)
	}
	socket, err := c.store.socket(m.ID, false)
	if err != nil || socket != m.Socket {
		return actionError(1, "invalid_socket", "private socket path is invalid", true, m.ID, name)
	}
	root := filepath.Dir(filepath.Dir(socket))
	dir := filepath.Dir(socket)
	if err = os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return actionError(1, "socket_repair_failed", "private socket directory could not be restored", true, m.ID, name)
	}
	if err = privateDir(root); err != nil {
		return actionError(1, "socket_repair_failed", "private socket directory ownership is invalid", true, m.ID, name)
	}
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return actionError(1, "socket_repair_failed", "private socket directory could not be restored", true, m.ID, name)
	}
	if err = privateDir(dir); err != nil {
		return actionError(1, "socket_repair_failed", "private socket directory ownership is invalid", true, m.ID, name)
	}
	fresh, err := identifyProcess(l.Server.PID)
	if err != nil || !sameIdentity(fresh, *l.Server) {
		return actionError(1, "identity_changed", "recorded server identity changed before repair", true, m.ID, name)
	}
	if err = syscall.Kill(l.Server.PID, syscall.SIGUSR1); err != nil {
		return actionError(1, "signal_failed", "socket repair signal was not delivered", true, m.ID, name)
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if snap, e := (tmux.Client{Executable: m.Tmux}).Inspect(ctx, m.Socket, "ac-"+m.ID); e == nil && snap.Owner == m.Owner && snap.Job == m.ID && snap.Protocol == m.Protocol && snap.ServerPID == l.Server.PID {
			if _, e = c.ownedSnapshot(ctx, m); e == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return actionError(75, "repair_unconfirmed", "socket repair unconfirmed; inspect doctor", true, m.ID, name)
		case <-deadline.C:
			return actionError(75, "repair_unconfirmed", "socket repair unconfirmed; inspect doctor", true, m.ID, name)
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) Forget(ctx context.Context, target string, byID bool) error {
	regCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unlockReg, err := c.store.lock(regCtx, "registry.lock")
	if err != nil {
		return err
	}
	defer unlockReg()
	id, err := c.resolve(target, byID)
	if err != nil {
		return err
	}
	unlockLaunch, err := c.store.lock(regCtx, filepath.Join("jobs", id, "launch.lock"))
	if err != nil {
		return err
	}
	defer unlockLaunch()
	m, l, err := c.store.load(id)
	if err != nil {
		return actionError(1, "ownership_unknown", "job ownership could not be verified", true, id, target)
	}
	communication, communicationErr := c.communicationLoad()
	if communicationErr != nil {
		return actionError(1, "communication_unknown", "communication ownership could not be verified; Forget refused", true, id, m.Name)
	}
	for _, binding := range communication.Bindings {
		if binding.JobID == id {
			return actionError(1, "communication_assigned", "release the worker from its manager before Forget", false, id, m.Name)
		}
	}
	for _, message := range communication.Messages {
		if message.Source.ID == id && communicationMessageBlocksForget(message) {
			return actionError(1, "communication_pending", "pending or uncertain communication prevents Forget", false, id, m.Name)
		}
	}
	if hasLegacyProcessEvidence(m, l) {
		return actionError(1, "legacy_identity", "claimed legacy work cannot be retired without a proven boot identity", true, id, m.Name)
	}
	removedTerminal := false
	if l.Phase == "exec-intent" {
		facts := c.facts(ctx, m, l)
		if facts.Launch == "submitted" || facts.Launch == "unreachable" || facts.Launch == "lost-terminal" || facts.Launch == "unknown" {
			return actionError(1, "live_or_uncertain", "possibly live job cannot be forgotten", true, id, m.Name)
		}
		if facts.Launch != "exited" {
			return actionError(1, "live_or_uncertain", "job state is not safely terminal", true, id, m.Name)
		}
		if snap, e := c.ownedSnapshot(ctx, m); e == nil {
			if snap.Clients != 0 {
				return actionError(1, "attached_clients", "detach all clients before forgetting this job", false, id, m.Name)
			}
			if !snap.PaneDead {
				return actionError(1, "live_job", "live pane cannot be forgotten", true, id, m.Name)
			}
			if e = c.removeDeadSession(ctx, m, &l, snap); e != nil {
				return actionError(1, "cleanup_failed", "owned dead session could not be safely removed", true, id, m.Name)
			}
			removedTerminal = true
		} else {
			server, se := compareProcess(*l.Server)
			pane, pe := compareProcess(*l.Pane)
			if se != nil || pe != nil || server != ProcessEnded || pane != ProcessEnded {
				return actionError(1, "ownership_unknown", "resources could not be proven ended", true, id, m.Name)
			}
			removedTerminal = true
		}
	} else if l.Phase == "failed" || l.Phase == "revoked" {
		if _, statErr := os.Lstat(m.Socket); statErr == nil {
			snap, snapshotErr := c.ownedSnapshot(ctx, m)
			if snapshotErr == nil {
				snapshotErr = c.removeDeadSession(ctx, m, &l, snap)
				removedTerminal = snapshotErr == nil
			} else {
				snapshotErr = c.removeStaleSocket(ctx, m, l)
			}
			if snapshotErr != nil {
				return actionError(1, "cleanup_refused", "retired resource is not verifiably safe to remove", true, id, m.Name)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return actionError(1, "ownership_unknown", "retired resource could not be verified", true, id, m.Name)
		}
	} else if l.Phase != "preexec" {
		return actionError(1, "unknown_phase", "job phase cannot be safely retired", true, id, m.Name)
	}
	l.Phase = "revoked"
	l.Retired = true
	if err := writeJSON(filepath.Join(c.store.root, "jobs", id), "launch.json", l); err != nil {
		return actionError(1, "retirement_failed", "job could not be durably revoked", true, id, m.Name)
	}
	if err := removeTask(filepath.Join(c.store.root, "jobs", id)); err != nil {
		return actionError(1, "retirement_failed", "task file could not be removed", true, id, m.Name)
	}
	if err := removeEndedControlEndpoint(ctx, m, l); err != nil {
		return actionError(1, "cleanup_pending", "job retired; controller endpoint cleanup remains unverified", true, id, m.Name)
	}
	if removedTerminal {
		if _, err := os.Lstat(m.Socket); err == nil {
			if err := c.removeStaleSocket(ctx, m, l); err != nil {
				return actionError(1, "cleanup_pending", "owned session retired; socket cleanup remains uncertain", true, id, m.Name)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return actionError(1, "cleanup_pending", "owned session retired; socket cleanup remains uncertain", true, id, m.Name)
		}
	}
	return nil
}

func removeEndedControlEndpoint(ctx context.Context, m Manifest, l Launch) error {
	path := controlEndpoint(m.Socket)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if l.Pane == nil || !identityEnded(*l.Pane) {
		return errors.New("controller process has not been proven ended")
	}
	before, err := checkSocketPath(path)
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", path)
	if err == nil {
		_ = conn.Close()
		return errors.New("controller endpoint still has a listener")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("controller endpoint listener state is unknown")
	}
	after, err := checkSocketPath(path)
	if err != nil || before.Dev != after.Dev || before.Ino != after.Ino {
		return errors.New("controller endpoint changed before cleanup")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (c *Coordinator) removeDeadSession(ctx context.Context, m Manifest, l *Launch, initial tmux.Snapshot) error {
	if initial.Clients != 0 || !initial.PaneDead || l.Pane != nil && l.Server == nil {
		return errors.New("session is live, attached, or lacks server identity")
	}
	boot, err := currentBoot()
	if err != nil || boot != m.BootID {
		return errors.New("server boot identity is not current")
	}
	firstServer, err := identifyProcess(initial.ServerPID)
	if err != nil || firstServer.BootID != boot || firstServer.UID != uint32(os.Getuid()) {
		return errors.New("server identity is not current-owner")
	}
	if l.Server != nil && !sameIdentity(firstServer, *l.Server) {
		return errors.New("recorded server identity disagrees")
	}
	if l.Pane != nil {
		paneStatus, paneErr := compareProcess(*l.Pane)
		if paneErr != nil || paneStatus != ProcessEnded || initial.PanePID != l.Pane.PID {
			return errors.New("recorded pane identity is not ended")
		}
	}
	freshSnapshot, err := c.ownedSnapshot(ctx, m)
	if err != nil || freshSnapshot.ServerPID != initial.ServerPID || freshSnapshot.PaneID != initial.PaneID || freshSnapshot.PanePID != initial.PanePID || !freshSnapshot.PaneDead || freshSnapshot.Clients != 0 {
		return errors.New("owned dead session changed before cleanup")
	}
	freshServer, err := identifyProcess(freshSnapshot.ServerPID)
	if err != nil || !sameIdentity(firstServer, freshServer) || freshServer.BootID != boot || freshServer.UID != uint32(os.Getuid()) {
		return errors.New("server identity changed before cleanup")
	}
	if l.Server == nil {
		l.Server = &freshServer
		if err := writeJSON(filepath.Join(c.store.root, "jobs", m.ID), "launch.json", l); err != nil {
			return err
		}
	}
	client := tmux.Client{Executable: m.Tmux}
	if m.Protocol == InteractiveProtocolVersion {
		if _, err := client.InspectTransparent(ctx, m.Socket, "ac-"+m.ID, "ac-return-"+m.ID); err != nil {
			return err
		}
		if err := client.Remove(ctx, m.Socket, "ac-return-"+m.ID); err != nil {
			return err
		}
		return client.Remove(ctx, m.Socket, "ac-"+m.ID)
	}
	return client.Remove(ctx, m.Socket, "ac-"+m.ID)
}

func (c *Coordinator) removeInteractiveResources(ctx context.Context, m Manifest, launch *Launch, snapshot tmux.Snapshot) error {
	if err := c.removeDeadSession(ctx, m, launch, snapshot); err != nil {
		return err
	}
	if err := removeEndedControlEndpoint(ctx, m, *launch); err != nil {
		return err
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Lstat(m.Socket); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err == nil {
			if err := c.removeStaleSocket(ctx, m, *launch); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("owned terminal server did not stop after its sessions were removed")
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) removeStaleSocket(ctx context.Context, m Manifest, l Launch) error {
	pathInfo, err := verifyStaleSocket(ctx, c.store, m, l)
	if err != nil {
		return err
	}
	if !identityEnded(*l.Server) || l.Pane != nil && !identityEnded(*l.Pane) {
		return errors.New("recorded server or pane may still be live")
	}
	currentInfo, err := checkSocketPath(m.Socket)
	if err != nil || currentInfo.Dev != pathInfo.Dev || currentInfo.Ino != pathInfo.Ino {
		return errors.New("socket path changed before removal")
	}
	if err := os.Remove(m.Socket); err != nil {
		return err
	}
	return syncDir(filepath.Dir(m.Socket))
}

func verifyStaleSocket(ctx context.Context, s *store, m Manifest, l Launch) (unix.Stat_t, error) {
	var empty unix.Stat_t
	if m.Owner != s.token || !supportedJobProtocol(m.Protocol) || l.Server == nil || !validProcessIdentity(*l.Server) {
		return empty, errors.New("recorded server identity unavailable")
	}
	socket, err := s.socket(m.ID, false)
	if err != nil || socket != m.Socket {
		return empty, errors.New("invalid recorded socket path")
	}
	pathInfo, err := checkSocketPath(socket)
	if err != nil {
		return empty, err
	}
	conn, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", socket)
	if err == nil {
		_ = conn.Close()
		return empty, errors.New("socket has a live listener")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return empty, errors.New("socket listener state is unknown")
	}
	return pathInfo, nil
}

func identityEnded(identity ProcessIdentity) bool {
	if !validProcessIdentity(identity) {
		return false
	}
	status, err := compareProcess(identity)
	return err == nil && status == ProcessEnded
}

func checkSocketPath(socket string) (unix.Stat_t, error) {
	var empty unix.Stat_t
	root, dir := filepath.Dir(filepath.Dir(socket)), filepath.Dir(socket)
	for _, path := range []string{root, dir} {
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0777 != 0700 || stat.Uid != uint32(os.Getuid()) {
			return empty, errors.New("socket parent is not a private current-owner directory")
		}
	}
	var stat unix.Stat_t
	if err := unix.Lstat(socket, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Getuid()) {
		return empty, errors.New("stale socket is not a current-owner socket")
	}
	return stat, nil
}
