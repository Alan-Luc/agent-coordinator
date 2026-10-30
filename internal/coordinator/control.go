package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"coordinate/internal/tmux"
	"golang.org/x/sys/unix"
)

const controlStatusMaxAge = 10 * time.Second
const controlAuthorizationTTL = 30 * time.Second
const controlWireLimit = 512 * 1024

func sessionLaunchProtocolMatches(status SessionStatus, protocol string) bool {
	return protocol == ProtocolVersion && status.LaunchProtocol == "" || protocol == InteractiveProtocolVersion && status.LaunchProtocol == InteractiveProtocolVersion
}

func validateSessionStatus(s SessionStatus, id, owner string, pid int, now time.Time) error {
	if s.Protocol != ControlProtocol || s.JobID != id || s.Owner != owner || s.PID != pid || s.SessionID == "" || len(s.SessionID) > 256 || !utf8.ValidString(s.SessionID) || !idPattern.MatchString(s.Generation) || s.UpdatedAt <= 0 || now.UnixMilli()-s.UpdatedAt > controlStatusMaxAge.Milliseconds() || s.UpdatedAt > now.Add(time.Second).UnixMilli() {
		return errors.New("session status identity or freshness mismatch")
	}
	if s.Activity != "unknown" && s.Activity != "idle" && s.Activity != "busy" {
		return errors.New("invalid session activity")
	}
	if s.Prompt != nil && (strings.TrimSpace(s.Prompt.Kind) == "" || len(s.Prompt.Kind) > 64 || len(s.Prompt.Title) > 1024) {
		return errors.New("invalid observed prompt")
	}
	if s.Report != nil && (!utf8.ValidString(s.Report.Summary) || strings.TrimSpace(s.Report.Summary) == "" || len(s.Report.Summary) > 1024 || s.Report.Attention != nil && (!utf8.ValidString(*s.Report.Attention) || strings.TrimSpace(*s.Report.Attention) == "" || len(*s.Report.Attention) > 1024)) {
		return errors.New("invalid agent report")
	}
	return nil
}

func (c *Coordinator) Preview(ctx context.Context, id string) (string, error) {
	m, launch, err := c.store.load(id)
	if err != nil || launch.Pane == nil || launch.Server == nil {
		return "", actionError(1, "target_unavailable", "session target is unavailable or unverified", true, id, "")
	}
	snap, err := c.ownedSnapshot(ctx, m)
	if err != nil || snap.PaneDead || snap.PaneID == "" || snap.PanePID != launch.Pane.PID {
		return "", actionError(1, "target_unavailable", "session target is unavailable or unverified", true, id, "")
	}
	pane, paneErr := identifyProcess(snap.PanePID)
	server, serverErr := identifyProcess(snap.ServerPID)
	if paneErr != nil || serverErr != nil || !sameIdentity(pane, *launch.Pane) || !sameIdentity(server, *launch.Server) {
		return "", actionError(1, "identity_changed", "session process identity changed", true, id, "")
	}
	text, err := (tmux.Client{Executable: m.Tmux}).Capture(ctx, m.Socket, snap.PaneID)
	if err != nil {
		return "", actionError(1, "preview_unavailable", "read-only session preview is unavailable", true, id, "")
	}
	return text, nil
}

func (c *Coordinator) ControlTarget(ctx context.Context, id string) (SessionTarget, error) {
	m, l, err := c.store.load(id)
	if err != nil || l.Phase != "exec-intent" || l.Server == nil || l.Pane == nil {
		return SessionTarget{}, actionError(1, "target_unavailable", "session target is unavailable or unverified", true, id, "")
	}
	snap, err := c.ownedSnapshot(ctx, m)
	if err != nil || snap.PaneDead || snap.PanePID != l.Pane.PID {
		return SessionTarget{}, actionError(1, "target_unavailable", "session target is unavailable or unverified", true, id, "")
	}
	server, se := identifyProcess(snap.ServerPID)
	pane, pe := identifyProcess(snap.PanePID)
	if se != nil || pe != nil || !sameIdentity(server, *l.Server) || !sameIdentity(pane, *l.Pane) {
		return SessionTarget{}, actionError(1, "identity_changed", "session process identity changed", true, id, "")
	}
	status, err := readSessionStatus(filepath.Join(c.store.root, "jobs", id, "session.json"), time.Now())
	if err != nil || c.validateJobSessionStatus(m, l, status, time.Now()) != nil {
		return SessionTarget{}, actionError(1, "session_unavailable", "fresh verified session control is unavailable", true, id, "")
	}
	return SessionTarget{ID: id, SessionID: status.SessionID, Generation: status.Generation, Pane: pane}, nil
}

func readSessionStatus(path string, now time.Time) (SessionStatus, error) {
	var status SessionStatus
	f, err := privateFile(path, unix.O_RDONLY)
	if err != nil {
		return status, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, controlWireLimit+1))
	if err != nil || len(b) > controlWireLimit {
		return status, errors.New("invalid session status size")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&status); err != nil {
		return status, errors.New("invalid session status")
	}
	if d.Decode(new(any)) != io.EOF {
		return status, errors.New("trailing session status")
	}
	if status.UpdatedAt <= 0 || now.UnixMilli()-status.UpdatedAt > controlStatusMaxAge.Milliseconds() || status.UpdatedAt > now.Add(time.Second).UnixMilli() {
		return status, errors.New("stale session status")
	}
	return status, nil
}

func validateControlText(text string) error {
	if !utf8.ValidString(text) || len(text) == 0 || len(text) > MaxControlTextBytes || strings.TrimSpace(text) == "" {
		return errors.New("control text must be nonblank UTF-8 within the size limit")
	}
	return nil
}

func (c *Coordinator) Send(ctx context.Context, target SessionTarget, requestID, text string) (ControlReceipt, error) {
	if !idPattern.MatchString(requestID) || validateControlText(text) != nil {
		return ControlReceipt{}, actionError(2, "invalid_request", "invalid request ID or message text", false, target.ID, "")
	}
	m, _, err := c.store.load(target.ID)
	if err != nil {
		return ControlReceipt{}, actionError(1, "target_unavailable", "session target unavailable", true, target.ID, "")
	}
	request := ControlRequest{Protocol: ControlProtocol, JobID: target.ID, SessionID: target.SessionID, Generation: target.Generation, RequestID: requestID, Action: "send", Text: text}
	return c.exchangeControl(ctx, m, target, request, 0)
}

func (c *Coordinator) exchangeControl(ctx context.Context, manifest Manifest, target SessionTarget, request ControlRequest, expiresAt int64) (ControlReceipt, error) {
	fresh, err := c.ControlTarget(ctx, target.ID)
	if err != nil || !sameTarget(fresh, target) {
		return ControlReceipt{}, actionError(1, "identity_changed", "session target changed before control dispatch", true, target.ID, "")
	}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", controlEndpoint(manifest.Socket))
	if err != nil {
		return ControlReceipt{}, actionError(75, "dispatch_uncertain", "control dispatch unavailable; request will not be retried", true, target.ID, "")
	}
	defer conn.Close()
	if err := requirePeerPID(conn, fresh.Pane.PID); err != nil {
		return ControlReceipt{}, actionError(1, "peer_mismatch", "control endpoint peer identity mismatch", true, target.ID, "")
	}
	fresh, err = c.ControlTarget(ctx, target.ID)
	if err != nil || !sameTarget(fresh, target) || expiresAt != 0 && expiresAt <= time.Now().UnixMilli() {
		return ControlReceipt{}, actionError(1, "identity_changed", "session target changed before control dispatch", true, target.ID, "")
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return ControlReceipt{}, actionError(75, "dispatch_uncertain", "control dispatch uncertain; request will not be retried", true, target.ID, "")
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok || unixConn.CloseWrite() != nil {
		return ControlReceipt{}, actionError(75, "dispatch_uncertain", "control dispatch uncertain; request will not be retried", true, target.ID, "")
	}
	line, err := io.ReadAll(io.LimitReader(conn, controlWireLimit+1))
	if err != nil || len(line) > controlWireLimit {
		return ControlReceipt{}, actionError(75, "dispatch_uncertain", "control receipt unavailable; request will not be retried", true, target.ID, "")
	}
	var response struct {
		Receipt *ControlReceipt `json:"receipt"`
		Error   *Diagnostic     `json:"error"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(line)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF {
		return ControlReceipt{}, actionError(75, "dispatch_uncertain", "control receipt unavailable; request will not be retried", true, target.ID, "")
	}
	if response.Error != nil {
		code := 1
		if response.Error.Uncertain {
			code = 75
		}
		return ControlReceipt{}, &ActionError{ExitCode: code, Diagnostic: Diagnostic{Code: response.Error.Code, Message: "control request was rejected", ID: target.ID, Uncertain: response.Error.Uncertain}}
	}
	if response.Receipt == nil || response.Receipt.ID != target.ID || response.Receipt.SessionID != target.SessionID || response.Receipt.Generation != target.Generation || response.Receipt.RequestID != request.RequestID || response.Receipt.Operation != request.Action || response.Receipt.Result != "api_invoked" {
		return ControlReceipt{}, actionError(75, "dispatch_uncertain", "control receipt identity mismatch; request will not be retried", true, target.ID, "")
	}
	return *response.Receipt, nil
}

func sameTarget(a, b SessionTarget) bool {
	return a.ID == b.ID && a.SessionID == b.SessionID && a.Generation == b.Generation && sameIdentity(a.Pane, b.Pane)
}

func (c *Coordinator) PrepareStop(ctx context.Context, target SessionTarget, force bool) (StopAuthorization, error) {
	if !idPattern.MatchString(target.ID) {
		return StopAuthorization{}, actionError(2, "invalid_target", "stop requires an immutable job UUID", false, "", "")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unlock, err := c.store.lock(lockCtx, filepath.Join("jobs", target.ID, "launch.lock"))
	if err != nil {
		return StopAuthorization{}, actionError(1, "authorization_busy", "stop authorization is busy", true, target.ID, "")
	}
	defer unlock()
	fresh, err := c.ControlTarget(ctx, target.ID)
	if err != nil || !sameTarget(fresh, target) {
		return StopAuthorization{}, actionError(1, "identity_changed", "session target changed; resolve and confirm again", true, target.ID, "")
	}
	op := "stop"
	if force {
		op = "force_stop"
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return StopAuthorization{}, actionError(1, "authorization_failed", "stop authorization unavailable", false, target.ID, "")
	}
	auth := StopAuthorization{Token: hex.EncodeToString(tokenBytes), Target: target, Operation: op, ExpiresAt: time.Now().Add(controlAuthorizationTTL).UnixMilli()}
	path := filepath.Join(c.store.root, "jobs", target.ID, "stop-authorization.json")
	if f, checkErr := privateFile(path, unix.O_RDONLY); checkErr == nil {
		_ = f.Close()
	} else if !errors.Is(checkErr, os.ErrNotExist) {
		return StopAuthorization{}, actionError(1, "authorization_invalid", "existing stop authorization is not a replaceable private file", true, target.ID, "")
	}
	if err := writeJSON(filepath.Dir(path), filepath.Base(path), auth); err != nil {
		return StopAuthorization{}, actionError(1, "authorization_failed", "stop authorization could not be atomically persisted", true, target.ID, "")
	}
	return auth, nil
}

func (c *Coordinator) ConfirmStop(ctx context.Context, auth StopAuthorization) (ControlReceipt, error) {
	token, tokenErr := hex.DecodeString(auth.Token)
	if auth.Operation != "stop" && auth.Operation != "force_stop" || tokenErr != nil || len(token) != 32 || auth.ExpiresAt <= time.Now().UnixMilli() {
		return ControlReceipt{}, actionError(2, "authorization_expired", "stop authorization is expired or invalid", false, auth.Target.ID, "")
	}
	fresh, err := c.ControlTarget(ctx, auth.Target.ID)
	if err != nil || !sameTarget(fresh, auth.Target) {
		return ControlReceipt{}, actionError(1, "identity_changed", "session target changed; no stop action taken", true, auth.Target.ID, "")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unlock, lockErr := c.store.lock(lockCtx, filepath.Join("jobs", auth.Target.ID, "launch.lock"))
	if lockErr != nil {
		return ControlReceipt{}, actionError(1, "authorization_busy", "stop authorization could not be consumed safely", true, auth.Target.ID, "")
	}
	defer unlock()
	if auth.ExpiresAt <= time.Now().UnixMilli() {
		return ControlReceipt{}, actionError(2, "authorization_expired", "stop authorization expired while waiting", false, auth.Target.ID, "")
	}
	fresh, err = c.ControlTarget(ctx, auth.Target.ID)
	if err != nil || !sameTarget(fresh, auth.Target) {
		return ControlReceipt{}, actionError(1, "identity_changed", "session target changed; no stop action taken", true, auth.Target.ID, "")
	}
	path := filepath.Join(c.store.root, "jobs", auth.Target.ID, "stop-authorization.json")
	var stored StopAuthorization
	if err := readJSON(path, &stored); err != nil || stored.Token != auth.Token || stored.Operation != auth.Operation || stored.ExpiresAt != auth.ExpiresAt || !sameTarget(stored.Target, auth.Target) {
		return ControlReceipt{}, actionError(2, "authorization_invalid", "stop authorization is invalid or already consumed", false, auth.Target.ID, "")
	}
	if err := os.Remove(path); err != nil {
		return ControlReceipt{}, actionError(1, "authorization_invalid", "stop authorization could not be consumed", true, auth.Target.ID, "")
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return ControlReceipt{}, actionError(1, "authorization_invalid", "stop authorization consumption is uncertain", true, auth.Target.ID, "")
	}
	if auth.Operation == "stop" {
		m, _, loadErr := c.store.load(auth.Target.ID)
		if loadErr != nil {
			return ControlReceipt{}, actionError(1, "target_unavailable", "graceful shutdown target is unavailable", true, auth.Target.ID, "")
		}
		requestID, idErr := newID()
		if idErr != nil {
			return ControlReceipt{}, actionError(1, "request_failed", "shutdown request could not be prepared", false, auth.Target.ID, "")
		}
		request := ControlRequest{Protocol: ControlProtocol, JobID: auth.Target.ID, SessionID: auth.Target.SessionID, Generation: auth.Target.Generation, RequestID: requestID, Action: "stop"}
		receipt, exchangeErr := c.exchangeControl(ctx, m, auth.Target, request, auth.ExpiresAt)
		if exchangeErr != nil {
			return ControlReceipt{}, exchangeErr
		}
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			checkCtx, cancelCheck := context.WithTimeout(ctx, 500*time.Millisecond)
			facts, doctorErr := c.DoctorID(checkCtx, auth.Target.ID)
			cancelCheck()
			if doctorErr == nil && facts.Launch == "exited" {
				receipt.Result = "exit_observed"
				return receipt, nil
			}
			select {
			case <-ctx.Done():
				return ControlReceipt{}, actionError(75, "shutdown_uncertain", "shutdown requested; exit was not observed", true, auth.Target.ID, "")
			case <-deadline.C:
				return ControlReceipt{}, actionError(75, "shutdown_uncertain", "shutdown requested; exit was not observed", true, auth.Target.ID, "")
			case <-ticker.C:
			}
		}
	}
	m, launch, loadErr := c.store.load(auth.Target.ID)
	if loadErr != nil || launch.Pane == nil || !sameIdentity(*launch.Pane, auth.Target.Pane) {
		return ControlReceipt{}, actionError(1, "identity_changed", "foreground identity changed; no signal sent", true, auth.Target.ID, "")
	}
	snap, snapErr := c.ownedSnapshot(ctx, m)
	if snapErr != nil || snap.PaneDead || snap.PanePID != auth.Target.Pane.PID {
		return ControlReceipt{}, actionError(1, "identity_changed", "owned foreground changed; no signal sent", true, auth.Target.ID, "")
	}
	requestID, err := newID()
	if err != nil {
		return ControlReceipt{}, actionError(1, "request_failed", "force-stop request could not be prepared", false, auth.Target.ID, "")
	}
	finalTarget, err := c.ControlTarget(ctx, auth.Target.ID)
	if err != nil || !sameTarget(finalTarget, auth.Target) || auth.ExpiresAt <= time.Now().UnixMilli() {
		return ControlReceipt{}, actionError(1, "identity_changed", "session generation or foreground changed; no signal sent", true, auth.Target.ID, "")
	}
	group, ge := processGroup(auth.Target.Pane.PID)
	foreground, fe := foregroundProcessGroup(auth.Target.Pane.PID)
	if ge != nil || fe != nil || group != auth.Target.Pane.PID || foreground != auth.Target.Pane.PID {
		return ControlReceipt{}, actionError(1, "unsafe_process_group", "force-stop requires the verified foreground Pi process group", true, auth.Target.ID, "")
	}
	last, err := identifyProcess(auth.Target.Pane.PID)
	if err != nil || !sameIdentity(last, auth.Target.Pane) {
		return ControlReceipt{}, actionError(1, "identity_changed", "foreground identity changed; no signal sent", true, auth.Target.ID, "")
	}
	if auth.ExpiresAt <= time.Now().UnixMilli() {
		return ControlReceipt{}, actionError(2, "authorization_expired", "stop authorization expired before signaling", false, auth.Target.ID, "")
	}
	if err := syscallKillGroup(auth.Target.Pane.PID); err != nil {
		return ControlReceipt{}, actionError(1, "signal_failed", "force-stop signal was not delivered", true, auth.Target.ID, "")
	}
	return ControlReceipt{ID: auth.Target.ID, SessionID: auth.Target.SessionID, Generation: auth.Target.Generation, RequestID: requestID, Operation: "force_stop", Result: "signal_sent"}, nil
}

func controlEndpoint(socket string) string { return filepath.Join(filepath.Dir(socket), "control") }
