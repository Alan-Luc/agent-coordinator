package coordinator

import (
	"context"
	"path/filepath"
	"time"
)

func (c *Coordinator) ReserveManagerHost(ctx context.Context, target SessionTarget, parentPID int) (JobRole, error) {
	refuse := func() (JobRole, error) {
		return JobRole{}, actionError(1, "manager_host_refused", "manager host requires this exact verified standalone Pi runtime", true, target.ID, "")
	}
	if parentPID <= 0 || target.Pane.PID != parentPID {
		return refuse()
	}
	identity, err := identifyProcess(parentPID)
	if err != nil || !sameIdentity(identity, target.Pane) {
		return refuse()
	}
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unlock, err := c.store.lock(lockCtx, "registry.lock")
	if err != nil {
		return JobRole{}, err
	}
	defer unlock()
	fresh, err := c.ControlTarget(lockCtx, target.ID)
	if err != nil || !sameTarget(fresh, target) {
		return refuse()
	}
	manifest, launch, err := c.store.load(target.ID)
	if err != nil || manifest.Protocol != InteractiveProtocolVersion {
		return refuse()
	}
	directory := filepath.Join(c.store.root, "jobs", target.ID)
	status, err := readSessionStatus(filepath.Join(directory, "session.json"), time.Now())
	if err != nil || status.SessionID != target.SessionID || status.Generation != target.Generation {
		return refuse()
	}
	if _, err := readNativeSessionForTarget(directory, manifest, launch, status, time.Now()); err != nil {
		return refuse()
	}
	role, err := readJobRole(directory)
	if err != nil {
		return refuse()
	}
	state, err := c.communicationLoad()
	if err != nil {
		return JobRole{}, err
	}
	for _, binding := range state.Bindings {
		if binding.JobID == target.ID {
			return JobRole{}, actionError(1, "worker_role", "a bound worker cannot become a manager host", false, target.ID, "")
		}
	}
	if role.Role == "manager-host" {
		return role, nil
	}
	for _, message := range state.Messages {
		if message.Source.ID == target.ID && communicationMessageBlocksRelease(message) {
			return JobRole{}, actionError(1, "requests_outstanding", "worker communication obligations prevent manager promotion", false, target.ID, "")
		}
	}
	role.Role = "manager-host"
	if err := writeJSON(directory, "role.json", role); err != nil {
		return JobRole{}, actionError(75, "role_uncertain", "manager host reservation may have changed; inspect before retrying", true, target.ID, "")
	}
	return role, nil
}

func (c *Coordinator) validateOwnedManagerCaller(ctx context.Context, caller CommunicationCaller, identity ProcessIdentity) error {
	ids, err := c.store.ids()
	if err != nil {
		return err
	}
	for _, id := range ids {
		manifest, launch, err := c.store.load(id)
		if launch.Pane == nil || !sameIdentity(*launch.Pane, identity) {
			continue
		}
		if err != nil {
			return actionError(1, "manager_host_unverified", "owned manager runtime records cannot be verified", true, id, "")
		}
		if manifest.Protocol != InteractiveProtocolVersion {
			continue
		}
		directory := filepath.Join(c.store.root, "jobs", id)
		role, err := readJobRole(directory)
		if err != nil || role.Role != "manager-host" {
			return actionError(1, "manager_host_required", "reserve this standalone runtime as a manager host first", false, id, "")
		}
		target, err := c.ControlTarget(ctx, id)
		if err != nil || target.SessionID != caller.SessionID || !sameIdentity(target.Pane, identity) {
			return actionError(1, "manager_host_unverified", "owned manager control is stale or unavailable", true, id, "")
		}
		status, err := readSessionStatus(filepath.Join(directory, "session.json"), time.Now())
		if err != nil {
			return err
		}
		native, err := readNativeSessionForTarget(directory, manifest, launch, status, time.Now())
		if err != nil || native.State != "persisted" || native.SessionFile != caller.SessionFile {
			return actionError(1, "manager_host_unverified", "owned manager conversation is not persisted and correlated", true, id, "")
		}
	}
	return nil
}
