package coordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordinate/internal/savedpi"
)

func resumeFixture(t *testing.T, dir, id string) savedpi.Snapshot {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, "saved.jsonl")
	body := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-10-29T00:00:00Z","cwd":"` + filepath.ToSlash(cwd) + `"}` + "\n" +
		`{"type":"message","id":"entry1","parentId":null,"timestamp":"2026-10-29T00:00:00Z","message":{"role":"user","content":"hello","timestamp":1793232000000}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := savedpi.Validate(path)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func withResumeLock(t *testing.T, c *Coordinator, run func()) {
	t.Helper()
	unlock, err := c.store.lock(t.Context(), "registry.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	run()
}

func TestResumeReservationRejectsSameTarget(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	snapshot := resumeFixture(t, filepath.Join(root, "target"), "native-id")
	launcher, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	var first ResumeReservation
	withResumeLock(t, c, func() {
		first, err = c.reserveSavedSessionLocked(context.Background(), "123e4567-e89b-42d3-a456-426614174099", "resume-test", &launcher, nil, snapshot)
		if err != nil {
			t.Fatal(err)
		}
	})
	if first.Phase != "reserved" || first.Initial == nil {
		t.Fatalf("unexpected reservation: %+v", first)
	}
	withResumeLock(t, c, func() {
		if _, err = c.reserveSavedSessionLocked(context.Background(), "123e4567-e89b-42d3-a456-426614174098", "resume-other", &launcher, nil, snapshot); err == nil {
			t.Fatal("same target must conflict")
		}
	})
}
func TestResumeReservationConflictsByNativeIDAndInode(t *testing.T) {
	left := savedpi.Snapshot{CanonicalFile: "/a", NativeID: "native", Device: 2, Inode: 3, Bytes: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if !snapshotConflict(left, savedpi.Snapshot{CanonicalFile: "/b", NativeID: "native", Device: 4, Inode: 5}) {
		t.Fatal("same native ID at different path did not conflict")
	}
	if !snapshotConflict(left, savedpi.Snapshot{CanonicalFile: "/b", NativeID: "other", Device: 2, Inode: 3}) {
		t.Fatal("same device/inode did not conflict")
	}
}
func TestResumeReservationSameHolderRetryRequiresSameEpochAndFingerprint(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	snapshot := resumeFixture(t, filepath.Join(root, "target"), "native-retry")
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	holder := SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174099", SessionID: "native-holder", Generation: "123e4567-e89b-42d3-a456-426614174098", Pane: identity}
	if err := c.ensureReservationRoot(); err != nil {
		t.Fatal(err)
	}
	reservation := ResumeReservation{Schema: 1, OperationID: "123e4567-e89b-42d3-a456-426614174096", Target: snapshot, Phase: "reserved", Switch: &ResumeSwitchActor{Holder: holder}}
	if err := writeJSON(c.reservationRoot(), reservation.OperationID+".json", reservation); err != nil {
		t.Fatal(err)
	}
	withResumeLock(t, c, func() {
		reused, e := c.reserveSavedSessionLocked(context.Background(), "", "", nil, &holder, snapshot)
		if e != nil || reused.OperationID != reservation.OperationID {
			t.Fatalf("same-holder retry: %+v %v", reused, e)
		}
		changed := holder
		changed.Generation = "123e4567-e89b-42d3-a456-426614174095"
		if _, e = c.reserveSavedSessionLocked(context.Background(), "", "", nil, &changed, snapshot); e == nil {
			t.Fatal("changed holder epoch reused reservation")
		}
	})
}
func TestResumeReservationRetiresOnlyEndedLaunchedBirth(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	snapshot := resumeFixture(t, filepath.Join(root, "target"), "native-ended")
	if err := c.ensureReservationRoot(); err != nil {
		t.Fatal(err)
	}
	ended := ProcessIdentity{BootID: "00000000-0000-0000-0000-000000000001", PID: 987654, UID: uint32(os.Getuid()), StartSec: 1}
	r := ResumeReservation{Schema: 1, OperationID: "123e4567-e89b-42d3-a456-426614174096", Target: snapshot, Phase: "launched", Initial: &ResumeLaunchActor{JobID: "123e4567-e89b-42d3-a456-426614174099", Name: "resume-test", Launcher: ended, Process: &ended}}
	if err := writeJSON(c.reservationRoot(), r.OperationID+".json", r); err != nil {
		t.Fatal(err)
	}
	launcher, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	withResumeLock(t, c, func() {
		if _, e := c.reserveSavedSessionLocked(context.Background(), "123e4567-e89b-42d3-a456-426614174098", "resume-new", &launcher, nil, snapshot); e != nil {
			t.Fatal(e)
		}
	})
	var retired ResumeReservation
	if err := readJSON(filepath.Join(c.reservationRoot(), r.OperationID+".json"), &retired); err != nil {
		t.Fatal(err)
	}
	if retired.Phase != "retired" {
		t.Fatalf("ended birth phase=%s", retired.Phase)
	}
}
func TestResumeReservationRejectsInvalidHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native-session-history.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"observations":[],"extra":true}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeHistory(path); err == nil {
		t.Fatal("accepted history with unknown field")
	}
	if err := os.WriteFile(path, make([]byte, maxNativeHistoryBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeHistory(path); err == nil {
		t.Fatal("accepted oversized history")
	}
}
func TestResumeReservationUncertainPreexecBirthRemainsBlocking(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := ResumeReservation{Schema: 1, OperationID: "123e4567-e89b-42d3-a456-426614174096", Target: savedpi.Snapshot{CanonicalFile: "/saved", NativeID: "native", Cwd: "/", Bytes: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Phase: "reserved", Initial: &ResumeLaunchActor{JobID: "123e4567-e89b-42d3-a456-426614174099", Name: "resume-test", Launcher: identity}}
	ended, err := c.reservationBirthEnded(r)
	if err != nil {
		t.Fatal(err)
	}
	if ended {
		t.Fatal("live launcher birth was treated as ended")
	}
}

func TestResumeReservationAcceptsNativeIDInHistory(t *testing.T) {
	o := NativeSessionObservation{Schema: 1, JobID: "123e4567-e89b-42d3-a456-426614174001", Owner: "0123456789abcdef", SessionID: "pi-session.native-1", Generation: "123e4567-e89b-42d3-a456-426614174002", Process: ProcessIdentity{BootID: "123e4567-e89b-42d3-a456-426614174003", PID: 10, UID: uint32(os.Getuid()), StartSec: 1}, UpdatedAt: time.Now(), SessionFile: "/tmp/s.jsonl", Cwd: "/tmp", State: "unpersisted"}
	body, err := json.Marshal(nativeHistory{Schema: 1, Observations: []NativeSessionObservation{o}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeHistory(path); err != nil {
		t.Fatalf("valid native session ID was rejected: %v", err)
	}
}
func TestResumeReservationFormerManagerConflictsByEitherKey(t *testing.T) {
	for _, tc := range []struct{ name, path, id string }{{"same native ID different path", "/old/path.jsonl", "same-id"}, {"same path changed ID", "target", "different-id"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s, err := openStore(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			c := &Coordinator{store: s}
			target := resumeFixture(t, filepath.Join(root, "target"), "same-id")
			managerPath := tc.path
			if managerPath == "target" {
				managerPath = target.CanonicalFile
			}
			state := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{{ID: "123e4567-e89b-42d3-a456-426614174081", SessionFile: managerPath, SessionID: tc.id}}, Bindings: []CommunicationBinding{}, Messages: []CommunicationMessage{}, Seen: []communicationSeen{}}
			if err := c.communicationSave(state); err != nil {
				t.Fatal(err)
			}
			if err := c.rejectOwnedHolders(target, nil); err == nil {
				t.Fatal("former manager path or native ID was not excluded")
			}
		})
	}
}
func TestResumeReservationHistoryCwdDoesNotRequireExistingDirectory(t *testing.T) {
	if err := canonicalHistoricalCwd(filepath.Join(t.TempDir(), "removed-cwd")); err != nil {
		t.Fatalf("lexically canonical historical cwd was rejected: %v", err)
	}
}

func TestResumeReservationManagerCurrentInodeConflicts(t *testing.T) {
	root := t.TempDir()
	manager := resumeFixture(t, filepath.Join(root, "manager"), "manager-native")
	target := resumeFixture(t, filepath.Join(root, "target"), "target-native")
	target.Device, target.Inode = manager.Device, manager.Inode
	if !managerConflictsTarget(CommunicationManager{SessionFile: manager.CanonicalFile, SessionID: manager.NativeID}, target) {
		t.Fatal("former manager current inode was not excluded")
	}
}

func TestResumeReservationPrivateRecordsRequireNativeSyntaxAndUUIDBirth(t *testing.T) {
	bad := savedpi.Snapshot{CanonicalFile: "/saved.jsonl", NativeID: "bad/id", Cwd: "/", Bytes: 1, SHA256: strings.Repeat("a", 64)}
	if validateSnapshot(bad) == nil {
		t.Fatal("accepted invalid native session ID")
	}
	actor := ResumeSwitchActor{Holder: SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174099", SessionID: "native-session", Generation: "123e4567-e89b-42d3-a456-426614174098", Pane: ProcessIdentity{BootID: "not-a-boot-uuid", PID: 1, UID: uint32(os.Getuid()), StartSec: 1}}}
	r := ResumeReservation{Schema: 1, OperationID: "123e4567-e89b-42d3-a456-426614174096", Target: savedpi.Snapshot{CanonicalFile: "/saved.jsonl", NativeID: "native-session", Cwd: "/", Bytes: 1, SHA256: strings.Repeat("a", 64)}, Phase: "reserved", Switch: &actor}
	if validateReservation(r) == nil {
		t.Fatal("accepted malformed process birth in private reservation")
	}
}
func TestResumeReservationSameHolderRetryRechecksManagerExclusion(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	snapshot := resumeFixture(t, filepath.Join(root, "target"), "native-retry-manager")
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	holder := SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174099", SessionID: "native-holder", Generation: "123e4567-e89b-42d3-a456-426614174098", Pane: identity}
	if err := c.ensureReservationRoot(); err != nil {
		t.Fatal(err)
	}
	r := ResumeReservation{Schema: 1, OperationID: "123e4567-e89b-42d3-a456-426614174096", Target: snapshot, Phase: "reserved", Switch: &ResumeSwitchActor{Holder: holder}}
	if err := writeJSON(c.reservationRoot(), r.OperationID+".json", r); err != nil {
		t.Fatal(err)
	}
	state := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{{ID: "123e4567-e89b-42d3-a456-426614174083", SessionFile: snapshot.CanonicalFile, SessionID: snapshot.NativeID}}, Bindings: []CommunicationBinding{}, Messages: []CommunicationMessage{}, Seen: []communicationSeen{}}
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	withResumeLock(t, c, func() {
		if _, e := c.reserveSavedSessionLocked(context.Background(), "", "", nil, &holder, snapshot); e == nil {
			t.Fatal("same-holder retry bypassed newly known former-manager key")
		}
	})
}

func TestResumeReservationRevokedLegacyNoPaneIsNotUnknown(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createLifecycleRecord(t, s, "123e4567-e89b-42d3-a456-426614174090", "revoked-test", "revoked", nil, nil); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	snapshot := resumeFixture(t, filepath.Join(root, "target"), "native-revoked")
	if err := c.rejectOwnedHolders(snapshot, nil); err != nil {
		t.Fatalf("proved revoked no-pane legacy job blocked resume: %v", err)
	}
}

func TestResumeReservationEndedHistoryDoesNotBlockKnownTarget(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	const job = "123e4567-e89b-42d3-a456-426614174090"
	previous := ProcessIdentity{BootID: "00000000-0000-0000-0000-000000000001", PID: 987654, UID: uint32(os.Getuid()), StartSec: 1}
	current, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	socket, err := createLifecycleRecord(t, s, job, "ended-history", "exec-intent", &current, &current)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "jobs", job)
	m, _, err := s.load(job)
	if err != nil {
		t.Fatal(err)
	}
	m.Protocol = InteractiveProtocolVersion
	m.BootID = previous.BootID
	if err := writeJSON(dir, "manifest.json", m); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "launch.json", Launch{Schema: SchemaVersion, Phase: "exec-intent", Server: &previous, Pane: &previous}); err != nil {
		t.Fatal(err)
	}
	_ = socket
	_ = os.Remove(filepath.Join(dir, "initial-task.txt"))
	managed := interactiveManagedFixture(t)
	if err := writeJSON(dir, "intent.json", InteractiveIntent{Schema: 1, Kind: "interactive", TerminalProfile: TransparentTerminalProfile, ArgvSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NativePi: &managed}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "role.json", JobRole{Schema: 1, Role: "standalone"}); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(dir, "launch-argv.json", []byte(`{"schema":1,"argv":[]}`)); err != nil {
		t.Fatal(err)
	}
	target := resumeFixture(t, filepath.Join(root, "target"), "native-ended-holder")
	row := NativeSessionObservation{Schema: 1, JobID: job, Owner: m.Owner, SessionID: target.NativeID, Generation: "123e4567-e89b-42d3-a456-426614174091", Process: previous, UpdatedAt: time.Now(), SessionFile: target.CanonicalFile, Cwd: target.Cwd, State: "persisted", File: &NativeFileIdentity{Device: target.Device, Inode: target.Inode}, HeaderID: target.NativeID}
	history := nativeHistory{Schema: 1, Observations: []NativeSessionObservation{row}}
	if err := writeJSON(dir, "native-session-history.json", history); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	if err := c.rejectOwnedHolders(target, nil); err != nil {
		t.Fatalf("ended runtime history incorrectly blocked target: %v", err)
	}
	state := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{{ID: "123e4567-e89b-42d3-a456-426614174081", SessionFile: target.CanonicalFile, SessionID: target.NativeID}}, Bindings: []CommunicationBinding{}, Messages: []CommunicationMessage{}, Seen: []communicationSeen{}}
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	t.Run("unrelated former manager does not exclude every target", func(t *testing.T) {
		unrelated := resumeFixture(t, filepath.Join(root, "unrelated"), "unrelated-session")
		if err := c.rejectOwnedHolders(unrelated, nil); err != nil {
			t.Fatalf("unrelated target blocked by manager history: %v", err)
		}
	})
	t.Run("former manager inode remains excluded after rename and ID change", func(t *testing.T) {
		moved := filepath.Join(filepath.Dir(target.CanonicalFile), "renamed.jsonl")
		if err := os.Rename(target.CanonicalFile, moved); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(moved)
		if err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(string(body), `"`+target.NativeID+`"`, `"renamed-session"`, 1)
		if err := os.WriteFile(moved, []byte(changed), 0644); err != nil {
			t.Fatal(err)
		}
		candidate, err := savedpi.Validate(moved)
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Inode != target.Inode || candidate.NativeID == target.NativeID || candidate.CanonicalFile == target.CanonicalFile {
			t.Fatal("fixture did not isolate historical inode exclusion")
		}
		if err := c.rejectOwnedHolders(candidate, nil); err == nil {
			t.Fatal("former manager historical inode was released")
		}
	})
}

func TestResumeReservationKnownBoundBirthSurvivesFailedNoPaneRecord(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	const job = "123e4567-e89b-42d3-a456-426614174094"
	if _, err := createLifecycleRecord(t, s, job, "known-bound", "failed", nil, nil); err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := ResumeReservation{Schema: 1, OperationID: "123e4567-e89b-42d3-a456-426614174095", Phase: "launched", Target: resumeFixture(t, filepath.Join(root, "saved"), "known-bound-target"), Initial: &ResumeLaunchActor{JobID: job, Name: "known-bound", Launcher: identity, Process: &identity}}
	if err := validateReservation(r); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	ended, err := c.reservationBirthEnded(r)
	if err != nil {
		t.Fatal(err)
	}
	if ended {
		t.Fatal("failed/no-pane metadata overrode a known live bound process")
	}
}

func TestResumeReservationEmptyHistoryCannotSkipCurrentSelfTarget(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	const job = "123e4567-e89b-42d3-a456-426614174092"
	_, err = createLifecycleRecord(t, s, job, "self-holder", "exec-intent", &identity, &identity)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "jobs", job)
	manifest, _, err := s.load(job)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Protocol = InteractiveProtocolVersion
	if err := writeJSON(dir, "manifest.json", manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "initial-task.txt")); err != nil {
		t.Fatal(err)
	}
	managed := interactiveManagedFixture(t)
	if err := writeJSON(dir, "intent.json", InteractiveIntent{Schema: 1, Kind: "interactive", TerminalProfile: TransparentTerminalProfile, ArgvSHA256: strings.Repeat("a", 64), NativePi: &managed}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "role.json", JobRole{Schema: 1, Role: "standalone"}); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(dir, "launch-argv.json", []byte(`{"schema":1,"argv":[]}`)); err != nil {
		t.Fatal(err)
	}
	target := resumeFixture(t, filepath.Join(root, "target"), "native-self")
	now := time.Now().UTC()
	status := SessionStatus{Protocol: 1, LaunchProtocol: InteractiveProtocolVersion, JobID: job, Owner: manifest.Owner, SessionID: target.NativeID, Generation: "123e4567-e89b-42d3-a456-426614174093", PID: identity.PID, UpdatedAt: now.UnixMilli(), Activity: "idle"}
	observation := NativeSessionObservation{Schema: 1, JobID: job, Owner: manifest.Owner, SessionID: target.NativeID, Generation: status.Generation, Process: identity, UpdatedAt: now, SessionFile: target.CanonicalFile, Cwd: target.Cwd, State: "persisted", File: &NativeFileIdentity{Device: target.Device, Inode: target.Inode}, HeaderID: target.NativeID}
	if err := writeJSON(dir, "session.json", status); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "native-session.json", observation); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dir, "native-session-history.json", nativeHistory{Schema: 1, Observations: []NativeSessionObservation{}}); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: s}
	holder := SessionTarget{ID: job, SessionID: status.SessionID, Generation: status.Generation, Pane: identity}
	if err := c.rejectOwnedHolders(target, &holder); err == nil {
		t.Fatal("current self-target bypassed conflict through allowed-source path")
	}
}
