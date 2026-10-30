package coordinator

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coordinate/internal/savedpi"
	"golang.org/x/sys/unix"
)

const resumeReservationDir = "resume-reservations"
const maxNativeHistoryBytes = 512 * 1024

type ResumeReservation struct {
	Schema      int                `json:"schema"`
	OperationID string             `json:"operation_id"`
	Target      savedpi.Snapshot   `json:"target"`
	Phase       string             `json:"phase"`
	Initial     *ResumeLaunchActor `json:"initial,omitempty"`
	Switch      *ResumeSwitchActor `json:"switch,omitempty"`
}

type ResumeLaunchActor struct {
	JobID    string           `json:"job_id"`
	Name     string           `json:"name"`
	Launcher ProcessIdentity  `json:"launcher"`
	Process  *ProcessIdentity `json:"process,omitempty"`
}

type ResumeSwitchActor struct {
	Holder SessionTarget `json:"holder"`
}

type nativeHistory struct {
	Schema       int                        `json:"schema"`
	Observations []NativeSessionObservation `json:"observations"`
}

func validateSnapshot(s savedpi.Snapshot) error {
	if !savedpi.ValidSessionID(s.NativeID) || !filepath.IsAbs(s.CanonicalFile) || filepath.Clean(s.CanonicalFile) != s.CanonicalFile || !filepath.IsAbs(s.Cwd) || filepath.Clean(s.Cwd) != s.Cwd || s.Bytes <= 0 || len(s.SHA256) != 64 {
		return errors.New("invalid saved-session snapshot")
	}
	if _, err := hex.DecodeString(s.SHA256); err != nil || strings.ToLower(s.SHA256) != s.SHA256 {
		return errors.New("invalid saved-session fingerprint")
	}
	return nil
}

func snapshotMatches(a, b savedpi.Snapshot) bool { return a == b }
func snapshotConflict(a, b savedpi.Snapshot) bool {
	return a.CanonicalFile == b.CanonicalFile || a.NativeID == b.NativeID || (a.Device == b.Device && a.Inode == b.Inode)
}

func readNativeHistory(path string) (nativeHistory, error) {
	var history nativeHistory
	f, err := privateFile(path, os.O_RDONLY)
	if err != nil {
		return history, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxNativeHistoryBytes+1))
	if err != nil || len(b) > maxNativeHistoryBytes {
		return history, errors.New("native history exceeds limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&history) != nil || decoder.Decode(new(any)) != io.EOF || history.Schema != 1 || history.Observations == nil || len(history.Observations) > 128 {
		return nativeHistory{}, errors.New("invalid native history")
	}
	for _, observation := range history.Observations {
		if observation.Schema != SchemaVersion || !idPattern.MatchString(observation.JobID) || !savedpi.ValidSessionID(observation.SessionID) || !idPattern.MatchString(observation.Generation) || !validProcessIdentity(observation.Process) || !bootUUIDPattern.MatchString(observation.Process.BootID) || !tokenPattern.MatchString(observation.Owner) || observation.UpdatedAt.IsZero() || !filepath.IsAbs(observation.SessionFile) || filepath.Clean(observation.SessionFile) != observation.SessionFile || canonicalHistoricalCwd(observation.Cwd) != nil {
			return nativeHistory{}, errors.New("invalid native history observation")
		}
		if observation.State != "unpersisted" && observation.State != "persisted" {
			return nativeHistory{}, errors.New("invalid native history state")
		}
		if observation.State == "persisted" && (observation.File == nil || observation.HeaderID != observation.SessionID) {
			return nativeHistory{}, errors.New("invalid persisted history tuple")
		}
		if observation.State == "unpersisted" && (observation.File != nil || observation.HeaderID != "") {
			return nativeHistory{}, errors.New("invalid unpersisted history tuple")
		}
	}
	return history, nil
}

func canonicalHistoricalCwd(cwd string) error {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return errors.New("noncanonical historical cwd")
	}
	return nil
}

func observationConflicts(o NativeSessionObservation, target savedpi.Snapshot) bool {
	return o.SessionFile == target.CanonicalFile || o.SessionID == target.NativeID || o.File != nil && o.File.Device == target.Device && o.File.Inode == target.Inode
}

func managerConflictsTarget(manager CommunicationManager, target savedpi.Snapshot) bool {
	if manager.SessionFile == target.CanonicalFile || manager.SessionID == target.NativeID {
		return true
	}
	device, inode, ok := currentSessionInode(manager.SessionFile)
	return ok && device == target.Device && inode == target.Inode
}

func managerConflictsObservation(manager CommunicationManager, observation NativeSessionObservation) bool {
	if manager.SessionFile == observation.SessionFile || manager.SessionID == observation.SessionID {
		return true
	}
	if observation.File == nil {
		return false
	}
	device, inode, ok := currentSessionInode(manager.SessionFile)
	return ok && device == observation.File.Device && inode == observation.File.Inode
}

func currentSessionInode(path string) (uint64, uint64, bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, 0, false
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true
}

func (c *Coordinator) reservationRoot() string {
	return filepath.Join(c.store.root, resumeReservationDir)
}
func (c *Coordinator) ensureReservationRoot() error { return privateDir(c.reservationRoot()) }

func (c *Coordinator) readReservations() ([]ResumeReservation, error) {
	root := c.reservationRoot()
	if err := c.ensureReservationRoot(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	reservations := make([]ResumeReservation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil, errors.New("unexpected reservation entry")
		}
		var reservation ResumeReservation
		if err := readJSON(filepath.Join(root, entry.Name()), &reservation); err != nil {
			return nil, err
		}
		if err := validateReservation(reservation); err != nil || entry.Name() != reservation.OperationID+".json" {
			return nil, errors.New("invalid saved-session reservation record")
		}
		reservations = append(reservations, reservation)
	}
	return reservations, nil
}

func validateReservation(r ResumeReservation) error {
	if r.Schema != 1 || !idPattern.MatchString(r.OperationID) || validateSnapshot(r.Target) != nil {
		return errors.New("invalid reservation")
	}
	if r.Phase != "reserved" && r.Phase != "launched" && r.Phase != "retired" {
		return errors.New("invalid reservation phase")
	}
	if (r.Initial == nil) == (r.Switch == nil) {
		return errors.New("reservation must have one actor")
	}
	if r.Initial != nil {
		if !idPattern.MatchString(r.Initial.JobID) || validateName(r.Initial.Name) != nil || !validProcessIdentity(r.Initial.Launcher) || !bootUUIDPattern.MatchString(r.Initial.Launcher.BootID) || r.Initial.Process != nil && (!validProcessIdentity(*r.Initial.Process) || !bootUUIDPattern.MatchString(r.Initial.Process.BootID)) {
			return errors.New("invalid launch actor")
		}
	} else {
		h := r.Switch.Holder
		if !idPattern.MatchString(h.ID) || !savedpi.ValidSessionID(h.SessionID) || !idPattern.MatchString(h.Generation) || !validProcessIdentity(h.Pane) || !bootUUIDPattern.MatchString(h.Pane.BootID) {
			return errors.New("invalid switch actor")
		}
	}
	return nil
}

func (c *Coordinator) reserveSavedSessionLocked(ctx context.Context, jobID, name string, launcher *ProcessIdentity, holder *SessionTarget, snapshot savedpi.Snapshot) (ResumeReservation, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return ResumeReservation{}, err
	}
	if err := savedpi.Recheck(snapshot); err != nil {
		return ResumeReservation{}, fmt.Errorf("saved session changed: %w", err)
	}
	if err := c.ensureReservationRoot(); err != nil {
		return ResumeReservation{}, err
	}
	reservations, err := c.readReservations()
	if err != nil {
		return ResumeReservation{}, err
	}
	var retry *ResumeReservation
	for i := range reservations {
		r := &reservations[i]
		if r.Phase == "retired" {
			continue
		}
		ended, proofErr := c.reservationBirthEnded(*r)
		if proofErr != nil {
			return ResumeReservation{}, proofErr
		}
		if ended {
			r.Phase = "retired"
			if err := writeJSON(c.reservationRoot(), r.OperationID+".json", *r); err != nil {
				return ResumeReservation{}, err
			}
			continue
		}
		if snapshotConflict(r.Target, snapshot) {
			if holder != nil && r.Switch != nil && sameTarget(r.Switch.Holder, *holder) && snapshotMatches(r.Target, snapshot) {
				candidate := *r
				retry = &candidate
				continue
			}
			return ResumeReservation{}, errors.New("saved session conflicts with an existing reservation")
		}
	}
	if err := c.rejectOwnedHolders(snapshot, holder); err != nil {
		return ResumeReservation{}, err
	}
	if err := savedpi.Recheck(snapshot); err != nil {
		return ResumeReservation{}, fmt.Errorf("saved session changed under registry lock: %w", err)
	}
	if retry != nil {
		return *retry, nil
	}
	op, err := newID()
	if err != nil {
		return ResumeReservation{}, err
	}
	r := ResumeReservation{Schema: 1, OperationID: op, Target: snapshot, Phase: "reserved"}
	if holder != nil {
		r.Switch = &ResumeSwitchActor{Holder: *holder}
	} else {
		if launcher == nil || !validProcessIdentity(*launcher) || !idPattern.MatchString(jobID) || validateName(name) != nil {
			return ResumeReservation{}, errors.New("initial reservation requires exact job, name and launcher birth")
		}
		current, currentErr := identifyProcess(launcher.PID)
		if currentErr != nil || !sameIdentity(current, *launcher) {
			return ResumeReservation{}, errors.New("initial reservation launcher birth is not current")
		}
		if _, _, loadErr := c.store.load(jobID); loadErr == nil {
			return ResumeReservation{}, errors.New("initial reservation job already exists")
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return ResumeReservation{}, loadErr
		}
		r.Initial = &ResumeLaunchActor{JobID: jobID, Name: name, Launcher: *launcher}
	}
	if err := validateReservation(r); err != nil {
		return ResumeReservation{}, err
	}
	if err := writeJSON(c.reservationRoot(), op+".json", r); err != nil {
		return ResumeReservation{}, err
	}
	return r, nil
}

func (c *Coordinator) reservationBirthEnded(r ResumeReservation) (bool, error) {
	var identity ProcessIdentity
	if r.Initial != nil {
		identity = r.Initial.Launcher
		if r.Initial.Process != nil {
			identity = *r.Initial.Process
		}
		if _, launch, err := c.store.load(r.Initial.JobID); err == nil {
			if launch.Pane != nil {
				if r.Initial.Process == nil {
					identity = *launch.Pane
				}
			} else if launch.Phase == "failed" || launch.Phase == "revoked" {
				if r.Initial.Process == nil {
					return true, nil
				}
			} else {
				return false, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	} else {
		identity = r.Switch.Holder.Pane
	}
	status, err := compareProcess(identity)
	return err == nil && status == ProcessEnded, nil
}

func (c *Coordinator) rejectOwnedHolders(target savedpi.Snapshot, allowed *SessionTarget) error {
	ids, err := c.store.ids()
	if err != nil {
		return err
	}
	state, err := c.communicationLoad()
	if err != nil {
		return err
	}
	for _, manager := range state.Managers {
		if managerConflictsTarget(manager, target) {
			return errors.New("saved session belongs to a former or enrolled manager")
		}
	}
	for _, id := range ids {
		manifest, launch, loadErr := c.store.load(id)
		if loadErr != nil {
			return loadErr
		}
		processStatus := ProcessUnknown
		if launch.Pane != nil {
			processStatus, err = compareProcess(*launch.Pane)
			if err != nil || processStatus == ProcessUnknown {
				return errors.New("owned runtime process birth is unknown")
			}
		} else if launch.Phase == "failed" || launch.Phase == "revoked" {
			processStatus = ProcessEnded
		} else if launch.Phase == "preexec" || launch.Phase == "exec-intent" {
			return errors.New("owned runtime launch phase is an unknown holder")
		}
		if manifest.Protocol != InteractiveProtocolVersion {
			if processStatus != ProcessEnded {
				return errors.New("legacy owned runtime is an unknown saved-session holder")
			}
			continue
		}
		jobDir := filepath.Join(c.store.root, "jobs", id)
		history, historyErr := readNativeHistory(filepath.Join(jobDir, "native-session-history.json"))
		if historyErr != nil {
			if !errors.Is(historyErr, os.ErrNotExist) {
				return errors.New("owned runtime native history is invalid")
			}
			if processStatus != ProcessEnded {
				return errors.New("owned runtime native history is missing")
			}
		} else {
			for _, observation := range history.Observations {
				if observation.JobID != id || observation.Owner != manifest.Owner || launch.Pane == nil || !sameIdentity(observation.Process, *launch.Pane) || !filepath.IsAbs(observation.SessionFile) || filepath.Clean(observation.SessionFile) != observation.SessionFile || canonicalHistoricalCwd(observation.Cwd) != nil {
					return errors.New("native history observation correlation is invalid")
				}
				for _, manager := range state.Managers {
					if managerConflictsObservation(manager, observation) && observationConflicts(observation, target) {
						return errors.New("saved session belongs to a former or enrolled manager")
					}
				}
				if processStatus != ProcessEnded && observationConflicts(observation, target) {
					return errors.New("saved session conflicts with an observed owned holder")
				}
			}
		}
		status, statusErr := readSessionStatus(filepath.Join(jobDir, "session.json"), time.Now())
		if statusErr != nil {
			if launch.Pane != nil {
				p, e := compareProcess(*launch.Pane)
				if e != nil || p != ProcessEnded {
					return errors.New("owned runtime current observation is unknown")
				}
			}
			continue
		}
		if processStatus == ProcessEnded {
			continue
		}
		observation, obsErr := readNativeSessionForTarget(jobDir, manifest, launch, status, time.Now())
		if obsErr != nil {
			return errors.New("owned runtime current observation is invalid")
		}
		for _, manager := range state.Managers {
			if managerConflictsObservation(manager, observation) && observationConflicts(observation, target) {
				return errors.New("saved session belongs to a former or enrolled manager")
			}
		}
		if observationConflicts(observation, target) {
			return errors.New("saved session conflicts with an owned current holder")
		}
		if allowed != nil && sameTarget(*allowed, SessionTarget{ID: id, SessionID: status.SessionID, Generation: status.Generation, Pane: observation.Process}) {
			continue
		}
	}
	return nil
}

func (c *Coordinator) ReserveSessionSwitch(ctx context.Context, holder SessionTarget, snapshot savedpi.Snapshot, parentPID int) (ResumeReservation, error) {
	if parentPID <= 0 || holder.Pane.PID != parentPID {
		return ResumeReservation{}, errors.New("switch caller is not the exact holder process")
	}
	identity, err := identifyProcess(parentPID)
	if err != nil || !sameIdentity(identity, holder.Pane) {
		return ResumeReservation{}, errors.New("switch caller birth identity mismatch")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unlock, err := c.store.lock(lockCtx, "registry.lock")
	if err != nil {
		return ResumeReservation{}, err
	}
	defer unlock()
	fresh, err := c.ControlTarget(lockCtx, holder.ID)
	if err != nil || !sameTarget(fresh, holder) {
		return ResumeReservation{}, errors.New("switch holder is stale")
	}
	manifest, launch, err := c.store.load(holder.ID)
	if err != nil || manifest.Protocol != InteractiveProtocolVersion {
		return ResumeReservation{}, errors.New("switch holder is not a supported owned runtime")
	}
	status, err := readSessionStatus(filepath.Join(c.store.root, "jobs", holder.ID, "session.json"), time.Now())
	if err != nil || status.SessionID != holder.SessionID || status.Generation != holder.Generation {
		return ResumeReservation{}, errors.New("switch holder status changed")
	}
	if _, err := readNativeSessionForTarget(filepath.Join(c.store.root, "jobs", holder.ID), manifest, launch, status, time.Now()); err != nil {
		return ResumeReservation{}, errors.New("switch holder native observation unavailable")
	}
	return c.reserveSavedSessionLocked(lockCtx, "", "", nil, &holder, snapshot)
}
