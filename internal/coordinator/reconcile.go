package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

func (c *Coordinator) facts(ctx context.Context, m Manifest, l Launch) JobFacts {
	f := JobFacts{ID: m.ID, Name: m.Name, Launch: l.Phase, Terminal: "unknown", PiReadiness: "unverified", TaskOutcome: "unknown", Retired: l.Retired}
	unknown := func(reason string) JobFacts {
		f.Launch = "unknown"
		f.NextAction = reason + "; inspect ownership and process identity; do not act"
		return f
	}
	if l.Schema != SchemaVersion || m.Schema != SchemaVersion || m.Owner != c.store.token || !supportedJobProtocol(m.Protocol) {
		return unknown("record schema or ownership mismatch")
	}
	if m.BootIDScheme != BootIdentityScheme || !bootUUIDPattern.MatchString(m.BootID) {
		return unknown("legacy or unsupported boot identity; native session may still be live")
	}
	if l.Phase == "preexec" {
		f.Launch = "starting"
		f.NextAction = "wait for launch or forget to revoke"
		return f
	}
	if l.Phase == "failed" {
		f.Launch = "failed"
		f.Terminal = "exited"
		f.NextAction = "forget failed launch"
		return f
	}
	if l.Phase == "revoked" {
		f.Launch = "revoked"
		if snap, err := c.ownedSnapshot(ctx, m); err == nil {
			f.Clients = snap.Clients
			if snap.PaneDead && snap.Clients == 0 {
				f.Terminal = "exited"
				f.ExitStatus = snap.ExitStatus
				f.NextAction = "forget retired residue by ID"
				return f
			}
			f.Terminal = "unknown"
		}
		f.NextAction = "inspect retired residue by ID"
		return f
	}
	if l.Phase != "exec-intent" || l.Server == nil || l.Pane == nil {
		return unknown("missing exec-intent process evidence")
	}
	if l.Server.BootID != m.BootID || l.Pane.BootID != m.BootID {
		f.Launch = "lost"
		f.Terminal = "unknown"
		f.NextAction = "inspect; previous boot identity"
		return f
	}
	snap, err := c.ownedSnapshot(ctx, m)
	if err == nil {
		server, se := identifyProcess(snap.ServerPID)
		if se != nil || !sameIdentity(server, *l.Server) {
			return unknown("server birth identity unavailable or changed")
		}
		f.Clients = snap.Clients
		if snap.PaneDead {
			paneStatus, paneErr := compareProcess(*l.Pane)
			if snap.PanePID != l.Pane.PID || paneErr != nil || paneStatus != ProcessEnded {
				return unknown("dead pane identity not proven ended")
			}
			f.Launch = "exited"
			f.Terminal = "exited"
			f.ExitStatus = snap.ExitStatus
			f.NextAction = "forget exited job"
			return f
		}
		pane, pe := identifyProcess(snap.PanePID)
		if pe != nil || !sameIdentity(pane, *l.Pane) {
			return unknown("live pane birth identity unavailable or changed")
		}
		f.Launch = "submitted"
		f.Terminal = "live"
		f.NextAction = "enter or observe"
		if status, statusErr := readSessionStatus(filepath.Join(c.store.root, "jobs", m.ID, "session.json"), time.Now()); statusErr == nil && c.validateJobSessionStatus(m, l, status, time.Now()) == nil {
			f.Session = &status
		}
		return f
	}
	socketPresent := false
	if _, statErr := os.Lstat(m.Socket); statErr == nil {
		if _, socketErr := verifyStaleSocket(ctx, c.store, m, l); socketErr != nil {
			return unknown("owned terminal query failed: " + err.Error())
		}
		socketPresent = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return unknown("owned terminal query failed: " + err.Error())
	}
	serverStatus, serverErr := compareProcess(*l.Server)
	paneStatus, paneErr := compareProcess(*l.Pane)
	if serverErr != nil || paneErr != nil || serverStatus == ProcessUnknown || paneStatus == ProcessUnknown {
		return unknown("recorded process identity unavailable")
	}
	if serverStatus == ProcessAlive && socketPresent {
		return unknown("recorded server is live but its socket path is stale")
	}
	if serverStatus == ProcessAlive {
		f.Launch = "unreachable"
		f.Terminal = "unreachable"
		f.NextAction = "repair-socket after confirming"
		return f
	}
	if paneStatus == ProcessAlive {
		f.Launch = "lost-terminal"
		f.Terminal = "lost"
		f.NextAction = "manual intervention; do not signal or delete"
		return f
	}
	f.Launch = "exited"
	f.Terminal = "exited"
	f.NextAction = "forget exited job"
	return f
}

func (c *Coordinator) List(ctx context.Context) (ListResult, error) {
	ids, err := c.store.ids()
	if err != nil {
		return ListResult{}, actionError(1, "registry_unavailable", "job registry is unavailable", false, "", "")
	}
	result := ListResult{Jobs: make([]JobFacts, 0, len(ids))}
	for _, id := range ids {
		m, l, e := c.store.load(id)
		if e != nil {
			result.Jobs = append(result.Jobs, JobFacts{ID: id, Launch: "unknown", Terminal: "unknown", PiReadiness: "unverified", TaskOutcome: "unknown", NextAction: "inspect invalid record; no actions safe"})
			continue
		}
		result.Jobs = append(result.Jobs, c.facts(ctx, m, l))
	}
	return result, nil
}

func (c *Coordinator) Doctor(ctx context.Context, name string) (JobFacts, error) {
	return c.doctor(ctx, name, false)
}

func (c *Coordinator) DoctorID(ctx context.Context, id string) (JobFacts, error) {
	return c.doctor(ctx, id, true)
}

func (c *Coordinator) doctor(ctx context.Context, target string, byID bool) (JobFacts, error) {
	id, err := c.resolve(target, byID)
	if err != nil {
		return JobFacts{}, err
	}
	m, l, err := c.store.load(id)
	if err != nil {
		return JobFacts{}, actionError(1, "ownership_unknown", "job ownership could not be verified", true, id, "")
	}
	return c.facts(ctx, m, l), nil
}

func (c *Coordinator) resolve(target string, byID bool) (string, error) {
	if byID {
		if !idPattern.MatchString(target) {
			return "", actionError(2, "invalid_id", "invalid job UUID", false, "", "")
		}
		if _, _, err := c.store.load(target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", actionError(1, "not_found", "job not found", false, target, "")
			}
			return "", actionError(1, "ownership_unknown", "job ownership could not be verified", true, target, "")
		}
		return target, nil
	}
	if validateName(target) != nil {
		return "", actionError(2, "invalid_name", "invalid job name", false, "", target)
	}
	ids, err := c.store.ids()
	if err != nil {
		return "", actionError(1, "registry_unavailable", "job registry is unavailable", false, "", target)
	}
	matchedID := ""
	for _, id := range ids {
		m, l, err := c.store.load(id)
		if err == nil && m.Name == target && (!l.Retired || hasLegacyProcessEvidence(m, l)) {
			if matchedID != "" {
				return "", actionError(1, "ambiguous_name", "multiple records reserve this name; use a job UUID", false, "", target)
			}
			matchedID = id
		}
	}
	if matchedID != "" {
		return matchedID, nil
	}
	return "", actionError(1, "not_found", "job not found", false, "", target)
}
func removeTask(dir string) error {
	err := os.Remove(filepath.Join(dir, "initial-task.txt"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
