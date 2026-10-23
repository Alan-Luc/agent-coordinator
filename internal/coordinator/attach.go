package coordinator

import (
	"context"
	"os"
	"os/exec"

	"agent-coordinator/internal/tmux"
)

func (c *Coordinator) Attach(ctx context.Context, name string, observe bool) (*exec.Cmd, error) {
	return c.attach(ctx, name, observe, false)
}

func (c *Coordinator) AttachID(ctx context.Context, id string, observe bool) (*exec.Cmd, error) {
	return c.attach(ctx, id, observe, true)
}

func (c *Coordinator) attach(ctx context.Context, name string, observe, byID bool) (*exec.Cmd, error) {
	if os.Getenv("TMUX") != "" {
		return nil, actionError(2, "inside_tmux", "Enter and Observe require an ordinary terminal", false, "", name)
	}
	facts, err := c.doctor(ctx, name, byID)
	if err != nil {
		return nil, err
	}
	if facts.Launch != "submitted" || facts.Terminal != "live" {
		return nil, actionError(1, "not_live", "job is not a verified live terminal", facts.Launch == "unknown", facts.ID, name)
	}
	id := facts.ID
	m, l, err := c.store.load(id)
	if err != nil {
		return nil, actionError(1, "ownership_unknown", "job ownership could not be verified", true, id, name)
	}
	snap, err := c.ownedSnapshot(ctx, m)
	if err != nil {
		return nil, actionError(1, "ownership_unknown", "owned tmux session could not be verified", true, id, name)
	}
	server, se := identifyProcess(snap.ServerPID)
	pane, pe := identifyProcess(snap.PanePID)
	if se != nil || pe != nil || l.Server == nil || l.Pane == nil || !sameIdentity(server, *l.Server) || !sameIdentity(pane, *l.Pane) || snap.PaneDead {
		return nil, actionError(1, "identity_changed", "job process identity changed", true, id, name)
	}
	return (tmux.Client{Executable: m.Tmux}).AttachCommand(ctx, m.Socket, "ac-"+id, observe), nil
}
