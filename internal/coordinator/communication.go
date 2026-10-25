package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type communicationSeen struct {
	ManagerID  string `json:"manager_id"`
	MessageID  string `json:"message_id"`
	Generation string `json:"generation"`
	SeenAt     int64  `json:"seen_at"`
}
type communicationState struct {
	Schema   int                    `json:"schema"`
	Protocol int                    `json:"protocol"`
	Managers []CommunicationManager `json:"managers"`
	Bindings []CommunicationBinding `json:"bindings"`
	Messages []CommunicationMessage `json:"messages"`
	Seen     []communicationSeen    `json:"seen"`
	Degraded bool                   `json:"degraded,omitempty"`
}

func (c *Coordinator) communicationLoad() (communicationState, error) {
	state := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{}, Bindings: []CommunicationBinding{}, Messages: []CommunicationMessage{}, Seen: []communicationSeen{}}
	path := filepath.Join(c.store.root, "communication", "state.json")
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	f, err := privateFile(path, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxCommunicationBytes+1))
	if err != nil || len(b) > MaxCommunicationBytes || !utf8.Valid(b) {
		return state, errors.New("communication document exceeds limit or is not UTF-8")
	}
	var shape map[string]json.RawMessage
	shapeDecoder := json.NewDecoder(strings.NewReader(string(b)))
	if shapeDecoder.Decode(&shape) != nil || shape == nil || shapeDecoder.Decode(new(any)) != io.EOF {
		return state, errors.New("unknown or invalid communication schema")
	}
	for _, field := range []string{"schema", "protocol", "managers", "bindings", "messages", "seen"} {
		if _, exists := shape[field]; !exists {
			return state, errors.New("unknown or invalid communication schema")
		}
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(&state) != nil || d.Decode(new(any)) != io.EOF || state.Schema != 1 || state.Protocol != CommunicationProtocol || validateCommunicationState(state) != nil {
		return state, errors.New("unknown or invalid communication schema")
	}
	return state, nil
}
func validateCommunicationState(s communicationState) error {
	if s.Managers == nil || s.Bindings == nil || s.Messages == nil || s.Seen == nil || len(s.Managers) > 16 || len(s.Bindings) > 128 || len(s.Messages) > 128 || len(s.Seen) > 2048 {
		return errors.New("communication bounds exceeded")
	}
	managers := map[string]bool{}
	conversations := map[string]bool{}
	for _, m := range s.Managers {
		conversation := m.SessionFile + "\x00" + m.SessionID
		if !idPattern.MatchString(m.ID) || m.SessionFile == "" || m.SessionID == "" || managers[m.ID] || conversations[conversation] {
			return errors.New("invalid manager")
		}
		managers[m.ID] = true
		conversations[conversation] = true
		if m.Lease != nil && (!idPattern.MatchString(m.Lease.Generation) || !validProcessIdentity(m.Lease.Process)) {
			return errors.New("invalid manager lease")
		}
	}
	bindings := map[string]bool{}
	for _, b := range s.Bindings {
		if !idPattern.MatchString(b.JobID) || !managers[b.ManagerID] || bindings[b.JobID] {
			return errors.New("invalid binding")
		}
		bindings[b.JobID] = true
	}
	messages := map[string]bool{}
	for _, m := range s.Messages {
		if !idPattern.MatchString(m.MessageID) || !managers[m.ManagerID] || !idPattern.MatchString(m.Source.ID) || m.Source.SessionID == "" || !idPattern.MatchString(m.Source.Generation) || !validProcessIdentity(m.Source.Pane) || !utf8.ValidString(m.Text) || len(m.Text) > MaxPublicationBytes || messages[m.MessageID] {
			return errors.New("invalid communication message")
		}
		messages[m.MessageID] = true
		if m.Kind != "question" && m.Kind != "blocker" && m.Kind != "result" || strings.TrimSpace(m.Text) == "" || m.CreatedAt <= 0 || m.AcknowledgedAt < 0 || m.ResolvedAt < 0 || m.EscalatedAt < 0 || m.AttemptGeneration != "" && !idPattern.MatchString(m.AttemptGeneration) || m.ResolutionNote != "" && validateCommunicationText(m.ResolutionNote) != nil || m.EscalationNote != "" && validateCommunicationText(m.EscalationNote) != nil {
			return errors.New("invalid communication message fields")
		}
		seenReplies := map[string]bool{}
		for _, reply := range m.Replies {
			if reply.Failure != nil && (reply.Receipt != nil || reply.Failure.Uncertain || reply.Failure.Code == "") {
				return errors.New("invalid saved reply outcome")
			}
			fingerprint, fingerprintErr := hex.DecodeString(reply.Fingerprint)
			if !idPattern.MatchString(reply.RequestID) || fingerprintErr != nil || len(fingerprint) != 32 || seenReplies[reply.RequestID] || !sameTarget(reply.Target, m.Source) {
				return errors.New("invalid saved reply")
			}
			seenReplies[reply.RequestID] = true
			if reply.Receipt != nil && (reply.Receipt.ID != reply.Target.ID || reply.Receipt.SessionID != reply.Target.SessionID || reply.Receipt.Generation != reply.Target.Generation || reply.Receipt.RequestID != reply.RequestID || reply.Receipt.Operation != "send" || reply.Receipt.Result != "api_invoked") {
				return errors.New("invalid saved reply receipt")
			}
		}
	}
	seen := map[string]bool{}
	for _, entry := range s.Seen {
		key := entry.ManagerID + "\x00" + entry.MessageID
		if !managers[entry.ManagerID] || !messages[entry.MessageID] || !idPattern.MatchString(entry.Generation) || entry.SeenAt <= 0 || seen[key] {
			return errors.New("invalid seen message")
		}
		seen[key] = true
	}
	return nil
}
func (c *Coordinator) communicationSave(s communicationState) error {
	if validateCommunicationState(s) != nil {
		return errors.New("invalid communication state")
	}
	s.Messages = append([]CommunicationMessage{}, s.Messages...)
	s.Seen = append([]communicationSeen{}, s.Seen...)
	pruneCommunicationMessages(&s, time.Now(), communicationDocumentWithinLimit)
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(b)+1 > MaxCommunicationBytes {
		return errors.New("communication document full")
	}
	dir := filepath.Join(c.store.root, "communication")
	if err := privateDir(dir); err != nil {
		return err
	}
	if err := writeBytes(dir, "state.json", append(b, '\n')); err != nil {
		return fmt.Errorf("%w: %w", actionError(75, "communication_persistence_uncertain", "Communication state may have changed; inspect it before retrying.", true, "", ""), err)
	}
	return nil
}
func (c *Coordinator) CommunicationMessages(ctx context.Context, cursor int) (CommunicationDiagnostics, error) {
	if cursor < 0 {
		return CommunicationDiagnostics{}, actionError(2, "invalid_cursor", "invalid message cursor", false, "", "")
	}
	unlock, err := c.store.lock(ctx, "registry.lock")
	if err != nil {
		return CommunicationDiagnostics{}, err
	}
	defer unlock()
	var d CommunicationDiagnostics
	d.Messages = []CommunicationMessage{}
	dir := filepath.Join(c.store.root, "communication")
	if err := checkPrivateDir(dir); errors.Is(err, os.ErrNotExist) {
		return d, nil
	} else if err != nil {
		return d, err
	}
	p := filepath.Join(dir, "state.json")
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	st, err := c.communicationLoad()
	if err != nil {
		return d, err
	}
	d.Pending, d.Escalated = communicationCounts(st.Messages)
	d.Degraded = st.Degraded
	visible := visibleCommunicationMessages(st.Messages, time.Now())
	d.Messages, d.NextCursor = communicationPage(visible, cursor)
	return d, nil
}
func (c *Coordinator) Communication(ctx context.Context, r CommunicationRequest, parentPID int) (CommunicationResult, error) {
	if r.Protocol != CommunicationProtocol {
		return CommunicationResult{}, actionError(2, "invalid_protocol", "unsupported communication protocol", false, "", "")
	}
	if r.Action == "capability" {
		if r.Caller != nil {
			return CommunicationResult{}, actionError(2, "invalid_request", "capability takes no caller", false, "", "")
		}
		return CommunicationResult{Capability: &CommunicationCapability{Protocol: CommunicationProtocol}}, nil
	}
	switch r.Action {
	case "enroll", "reconnect", "pause", "disconnect", "adopt", "release", "start", "publish", "inbox", "attempt", "ack", "escalate", "resolve", "reply":
	default:
		return CommunicationResult{}, actionError(2, "invalid_action", "unsupported communication action", false, "", "")
	}
	if r.Caller == nil || r.Caller.PID != parentPID || parentPID <= 0 {
		return CommunicationResult{}, actionError(1, "caller_mismatch", "communication caller identity mismatch", false, "", "")
	}
	identity, err := identifyProcess(parentPID)
	if err != nil || !validProcessIdentity(identity) {
		return CommunicationResult{}, actionError(1, "identity_unknown", "caller process identity unavailable", true, "", "")
	}
	managerAction := r.Action == "enroll" || r.Action == "reconnect" || r.Action == "pause" || r.Action == "disconnect" || r.Action == "adopt" || r.Action == "release" || r.Action == "start" || r.Action == "inbox" || r.Action == "attempt" || r.Action == "ack" || r.Action == "reply" || r.Action == "escalate" || r.Action == "resolve" && r.Caller.JobID == ""
	if managerAction && r.Caller.JobID != "" {
		return CommunicationResult{}, actionError(2, "invalid_caller", "manager action cannot select a job caller", false, "", "")
	}
	if r.Action == "publish" && r.Caller.JobID == "" {
		return CommunicationResult{}, actionError(2, "invalid_caller", "worker action requires a bound job caller", false, "", "")
	}
	if managerAction {
		if err := validManagerConversation(*r.Caller); err != nil {
			return CommunicationResult{}, actionError(1, "invalid_manager", "manager conversation is not persisted and verified", false, "", "")
		}
	}
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unlock, err := c.store.lock(lockCtx, "registry.lock")
	if err != nil {
		return CommunicationResult{}, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	if err := c.reapCommunicationTemps(); err != nil {
		return CommunicationResult{}, err
	}
	s, err := c.communicationLoad()
	if err != nil {
		return CommunicationResult{}, err
	}
	if r.Action == "enroll" || r.Action == "reconnect" || r.Action == "pause" || r.Action == "disconnect" {
		if err := validManagerConversation(*r.Caller); err != nil {
			return CommunicationResult{}, actionError(1, "invalid_manager", "manager conversation is not persisted and verified", false, "", "")
		}
		idx := -1
		for i := range s.Managers {
			if s.Managers[i].SessionFile == r.Caller.SessionFile && s.Managers[i].SessionID == r.Caller.SessionID {
				idx = i
			}
		}
		if idx < 0 && r.Action == "enroll" {
			if len(s.Managers) >= 16 {
				s.Degraded = true
				if err = c.communicationSave(s); err != nil {
					return CommunicationResult{}, err
				}
				return CommunicationResult{}, actionError(1, "enrollment_refused", "manager capacity full", false, "", "")
			}
			pruneExpiredCommunicationMessages(&s, time.Now())
			id, e := newID()
			if e != nil {
				return CommunicationResult{}, e
			}
			m := CommunicationManager{ID: id, SessionFile: r.Caller.SessionFile, SessionID: r.Caller.SessionID, Enabled: true, Lease: &CommunicationLease{Generation: r.Caller.Generation, Process: identity}}
			s.Managers = append(s.Managers, m)
			if err = c.communicationSave(s); err != nil {
				return CommunicationResult{}, err
			}
			return CommunicationResult{Manager: &m}, nil
		}
		if idx < 0 {
			return CommunicationResult{}, actionError(1, "manager_unknown", "manager is not enrolled", false, "", "")
		}
		m := &s.Managers[idx]
		exactLease := m.Lease != nil && m.Lease.Generation == r.Caller.Generation && sameIdentity(m.Lease.Process, identity)
		if r.Action == "enroll" {
			if m.Lease != nil && !exactLease {
				if m.Lease.Generation == r.Caller.Generation {
					return CommunicationResult{}, actionError(1, "lease_mismatch", "same generation has a different process birth identity", true, "", "")
				}
				status, e := compareProcess(m.Lease.Process)
				if e != nil || status != ProcessEnded {
					return CommunicationResult{}, actionError(1, "lease_owned", "manager runtime is live or unknown", true, "", "")
				}
			}
			m.Enabled = true
			m.Lease = &CommunicationLease{Generation: r.Caller.Generation, Process: identity}
		} else if r.Action == "reconnect" {
			if m.Lease != nil && !exactLease {
				if m.Lease.Generation == r.Caller.Generation {
					return CommunicationResult{}, actionError(1, "lease_mismatch", "same generation has a different process birth identity", true, "", "")
				}
				status, e := compareProcess(m.Lease.Process)
				if e != nil || status != ProcessEnded {
					return CommunicationResult{}, actionError(1, "lease_owned", "manager runtime is live or unknown", true, "", "")
				}
			}
			m.Lease = &CommunicationLease{Generation: r.Caller.Generation, Process: identity}
		} else {
			if !exactLease {
				return CommunicationResult{}, actionError(1, "lease_mismatch", "manager operation does not own exact runtime lease", true, "", "")
			}
			if r.Action == "pause" {
				m.Enabled = false
			} else {
				m.Lease = nil
			}
		}
		pruneExpiredCommunicationMessages(&s, time.Now())
		if err = c.communicationSave(s); err != nil {
			return CommunicationResult{}, err
		}
		return CommunicationResult{Manager: m}, nil
	}
	mi := -1
	if r.Caller != nil {
		for i := range s.Managers {
			m := &s.Managers[i]
			if m.SessionFile == r.Caller.SessionFile && m.SessionID == r.Caller.SessionID && m.Lease != nil && m.Lease.Generation == r.Caller.Generation && sameIdentity(m.Lease.Process, identity) {
				mi = i
				break
			}
		}
	}
	var manager *CommunicationManager
	if mi >= 0 {
		manager = &s.Managers[mi]
		if pruneExpiredCommunicationMessages(&s, time.Now()) {
			if err = c.communicationSave(s); err != nil {
				return CommunicationResult{}, err
			}
		}
	}
	if r.Action == "resolve" && mi < 0 && r.Caller.JobID != "" {
		target, targetErr := c.ControlTarget(ctx, r.Caller.JobID)
		if targetErr != nil || target.SessionID != r.Caller.SessionID || target.Generation != r.Caller.Generation || target.Pane.PID != parentPID {
			return CommunicationResult{}, actionError(1, "caller_mismatch", "worker controller identity mismatch", true, r.Caller.JobID, "")
		}
		for i := range s.Messages {
			m := &s.Messages[i]
			if m.MessageID == r.MessageID && sameTarget(m.Source, target) {
				if validateCommunicationNote(r.Note) != nil {
					return CommunicationResult{}, actionError(2, "invalid_note", "resolution note must be nonblank UTF-8 and at most 4 KiB", false, r.Caller.JobID, "")
				}
				m.ResolvedAt = time.Now().UnixMilli()
				m.ResolutionNote = r.Note
				if err = c.communicationSave(s); err != nil {
					return CommunicationResult{}, err
				}
				return CommunicationResult{Message: m}, nil
			}
		}
		return CommunicationResult{}, actionError(1, "message_unknown", "worker may resolve only its own message", false, r.Caller.JobID, "")
	}
	switch r.Action {
	case "inbox", "attempt", "ack", "reply", "resolve", "escalate":
		if mi < 0 {
			return CommunicationResult{}, actionError(1, "manager_lease_required", "active manager lease required", false, "", "")
		}
		if r.Action == "inbox" {
			if !manager.Enabled {
				return CommunicationResult{}, actionError(1, "manager_paused", "manager delivery is paused", false, "", "")
			}
			if r.Cursor < 0 {
				return CommunicationResult{}, actionError(2, "invalid_cursor", "invalid inbox cursor", false, "", "")
			}
			owned := []CommunicationMessage{}
			for _, message := range s.Messages {
				if message.ManagerID == manager.ID {
					owned = append(owned, message)
				}
			}
			pending, escalated := communicationCounts(owned)
			msgs, next := communicationPage(owned, r.Cursor)
			needsSeen := false
			for _, message := range msgs {
				if !communicationMessageSeen(s, manager.ID, r.Caller.Generation, message.MessageID) {
					needsSeen = true
					break
				}
			}
			if needsSeen {
				if err = markCommunicationMessagesSeen(&s, manager.ID, r.Caller.Generation, msgs, time.Now()); err != nil {
					return CommunicationResult{}, err
				}
				if err = c.communicationSave(s); err != nil {
					return CommunicationResult{}, err
				}
			}
			for i := range msgs {
				msgs[i].Replies = nil
			}
			unlock()
			locked = false
			workers := []SessionStatus{}
			if r.Cursor == 0 {
				workers = c.communicationWorkers(ctx, s, manager.ID)
			}
			return CommunicationResult{Inbox: &CommunicationInbox{Messages: msgs, NextCursor: next, Pending: pending, Escalated: escalated, Degraded: s.Degraded, Workers: workers}}, nil
		}
		if r.Action == "ack" {
			if len(r.MessageIDs) == 0 || len(r.MessageIDs) > CommunicationPageSize {
				return CommunicationResult{}, actionError(2, "invalid_ack", "ack requires message IDs", false, "", "")
			}
			changed := false
			out := []CommunicationMessage{}
			seen := map[string]bool{}
			for _, id := range r.MessageIDs {
				if seen[id] {
					return CommunicationResult{}, actionError(2, "invalid_ack", "ack message IDs must be unique", false, "", "")
				}
				seen[id] = true
				if !communicationMessageSeen(s, manager.ID, r.Caller.Generation, id) {
					return CommunicationResult{}, actionError(1, "message_unseen", "cannot acknowledge a message that has not been shown in inbox", false, "", "")
				}
				found := false
				for i := range s.Messages {
					if s.Messages[i].ManagerID == manager.ID && s.Messages[i].MessageID == id {
						s.Messages[i].AcknowledgedAt = time.Now().UnixMilli()
						out = append(out, s.Messages[i])
						found = true
						changed = true
						break
					}
				}
				if !found {
					return CommunicationResult{}, actionError(1, "message_unknown", "ack includes a message not owned by manager", false, "", "")
				}
			}
			if changed {
				if err = c.communicationSave(s); err != nil {
					return CommunicationResult{}, err
				}
			}
			return CommunicationResult{Messages: &out}, nil
		}
		if r.Action == "attempt" {
			if len(r.MessageIDs) == 0 || len(r.MessageIDs) > CommunicationPageSize {
				return CommunicationResult{}, actionError(2, "invalid_attempt", "attempt requires at most one page of IDs", false, "", "")
			}
			seenIDs := map[string]bool{}
			for _, id := range r.MessageIDs {
				if !idPattern.MatchString(id) || seenIDs[id] {
					return CommunicationResult{}, actionError(2, "invalid_attempt", "message IDs must be valid and unique", false, "", "")
				}
				seenIDs[id] = true
				owned := false
				for _, message := range s.Messages {
					if message.ManagerID == manager.ID && message.MessageID == id {
						owned = true
						break
					}
				}
				if !owned {
					return CommunicationResult{}, actionError(1, "message_unknown", "attempt includes a message not owned by manager", false, "", "")
				}
			}
			if !manager.Enabled {
				return CommunicationResult{}, actionError(1, "manager_paused", "manager delivery is paused", false, "", "")
			}
			out, changed := claimCommunicationAttempts(s.Messages, manager.ID, r.Caller.Generation, r.MessageIDs)
			if changed {
				if err = c.communicationSave(s); err != nil {
					return CommunicationResult{}, err
				}
			}
			return CommunicationResult{Messages: &out}, nil
		}
		for i := range s.Messages {
			m := &s.Messages[i]
			if m.ManagerID != manager.ID || m.MessageID != r.MessageID {
				continue
			}
			if !communicationMessageSeen(s, manager.ID, r.Caller.Generation, m.MessageID) {
				return CommunicationResult{}, actionError(1, "message_unseen", "message must be read from inbox before acting", false, "", "")
			}
			now := time.Now().UnixMilli()
			switch r.Action {
			case "resolve":
				if validateCommunicationNote(r.Note) != nil {
					return CommunicationResult{}, actionError(2, "invalid_note", "resolution note must be nonblank UTF-8 and at most 4 KiB", false, m.Source.ID, "")
				}
				m.ResolvedAt = now
				m.ResolutionNote = r.Note
			case "escalate":
				if validateCommunicationNote(r.Note) != nil {
					return CommunicationResult{}, actionError(2, "invalid_note", "escalation note must be nonblank UTF-8 and at most 4 KiB", false, m.Source.ID, "")
				}
				m.EscalatedAt = now
				m.EscalationNote = r.Note
			case "reply":
				if !manager.Enabled {
					return CommunicationResult{}, actionError(1, "manager_paused", "manager delivery is paused", false, m.Source.ID, "")
				}
				if !idPattern.MatchString(r.RequestID) || validateCommunicationText(r.Text) != nil {
					return CommunicationResult{}, actionError(2, "invalid_reply", "reply requires a UUID and valid text", false, m.Source.ID, "")
				}
				fingerprint := messageFingerprint(r.Text)
				savedReceipt, claimErr := prepareCommunicationReply(m, r.RequestID, r.Text, m.Source)
				if claimErr != nil {
					return CommunicationResult{}, claimErr
				}
				if savedReceipt != nil {
					return CommunicationResult{Receipt: savedReceipt}, nil
				}
				if m.Kind == "result" || m.ResolvedAt != 0 {
					return CommunicationResult{}, actionError(1, "reply_not_allowed", "replies require an unresolved question or blocker", false, m.Source.ID, "")
				}
				if !reserveCommunicationReply(&s, time.Now()) {
					m.Replies = m.Replies[:len(m.Replies)-1]
					s.Degraded = true
					if err = c.communicationSave(s); err != nil {
						return CommunicationResult{}, err
					}
					return CommunicationResult{}, actionError(1, "queue_full", "reply capacity is full; no send was dispatched", false, m.Source.ID, "")
				}
				fresh, targetErr := c.ControlTarget(ctx, m.Source.ID)
				if targetErr != nil || !sameTarget(fresh, m.Source) {
					return CommunicationResult{}, actionError(1, "identity_changed", "saved worker target is stale", true, m.Source.ID, "")
				}

				if err = c.communicationSave(s); err != nil {
					return CommunicationResult{}, err
				}
				savedTarget := m.Source
				unlock()
				locked = false
				receipt, sendErr := c.Send(ctx, savedTarget, r.RequestID, "Coordinator reply to "+r.MessageID+":\n"+r.Text)
				var failure *Diagnostic
				if sendErr != nil {
					var rejected *ActionError
					if !errors.As(sendErr, &rejected) || rejected.Diagnostic.Uncertain {
						return CommunicationResult{}, sendErr
					}
					failure = &rejected.Diagnostic
				}
				receiptCtx, receiptCancel := context.WithTimeout(ctx, 3*time.Second)
				defer receiptCancel()
				relock, lockErr := c.store.lock(receiptCtx, "registry.lock")
				if lockErr != nil {
					return CommunicationResult{}, actionError(75, "reply_receipt_uncertain", "reply was invoked but receipt persistence is unavailable", true, savedTarget.ID, "")
				}
				locked = true
				unlock = relock
				latest, loadErr := c.communicationLoad()
				if loadErr != nil {
					return CommunicationResult{}, actionError(75, "reply_receipt_uncertain", "reply was invoked but receipt persistence is unavailable", true, savedTarget.ID, "")
				}
				managerStillOwns := false
				for _, current := range latest.Managers {
					if current.ID == manager.ID && current.SessionFile == r.Caller.SessionFile && current.SessionID == r.Caller.SessionID && current.Lease != nil && current.Lease.Generation == r.Caller.Generation && sameIdentity(current.Lease.Process, identity) {
						managerStillOwns = true
					}
				}
				bindingStillOwned := false
				for _, binding := range latest.Bindings {
					if binding.JobID == savedTarget.ID && binding.ManagerID == manager.ID {
						bindingStillOwned = true
					}
				}
				if !managerStillOwns || !bindingStillOwned {
					return CommunicationResult{}, actionError(75, "reply_receipt_uncertain", "reply was invoked after manager ownership changed", true, savedTarget.ID, "")
				}
				for i := range latest.Messages {
					if latest.Messages[i].ManagerID == manager.ID && latest.Messages[i].MessageID == r.MessageID {
						for j := range latest.Messages[i].Replies {
							claim := &latest.Messages[i].Replies[j]
							if claim.RequestID == r.RequestID && claim.Fingerprint == fingerprint && sameTarget(claim.Target, savedTarget) {
								if failure != nil {
									claim.Failure = failure
								} else {
									claim.Receipt = &receipt
								}
								if err = c.communicationSave(latest); err != nil {
									return CommunicationResult{}, actionError(75, "reply_receipt_uncertain", "reply was invoked but receipt persistence is unavailable", true, savedTarget.ID, "")
								}
								if failure != nil {
									return CommunicationResult{}, sendErr
								}
								return CommunicationResult{Receipt: &receipt}, nil
							}
						}
					}
				}
				return CommunicationResult{}, actionError(75, "reply_receipt_uncertain", "reply was invoked but its saved claim changed", true, savedTarget.ID, "")
			}
			if err = c.communicationSave(s); err != nil {
				return CommunicationResult{}, err
			}
			return CommunicationResult{Message: m}, nil
		}
		return CommunicationResult{}, actionError(1, "message_unknown", "message is not owned by manager", false, "", "")
	case "adopt":
		if manager == nil {
			return CommunicationResult{}, actionError(1, "manager_lease_required", "active manager lease required", false, "", "")
		}
		if r.JobID == "" || r.Target == nil {
			return CommunicationResult{}, actionError(2, "invalid_adoption", "adoption requires a job and confirmed target", false, r.JobID, "")
		}
		if !sameSessionCaller(*manager, *r.Caller) {
			return CommunicationResult{}, actionError(1, "manager_lease_required", "active manager lease required", false, r.JobID, "")
		}
		fresh, e := c.ControlTarget(ctx, r.JobID)
		if e != nil || !sameTarget(fresh, *r.Target) {
			return CommunicationResult{}, actionError(1, "identity_changed", "confirmed worker target changed", true, r.JobID, "")
		}
		for _, b := range s.Bindings {
			if b.JobID == r.JobID {
				return CommunicationResult{}, actionError(1, "already_assigned", "worker already has a manager", false, r.JobID, "")
			}
		}
		if len(s.Bindings) >= 128 {
			s.Degraded = true
			if err = c.communicationSave(s); err != nil {
				return CommunicationResult{}, err
			}
			return CommunicationResult{}, actionError(1, "queue_full", "binding capacity is full", false, r.JobID, "")
		}
		b := CommunicationBinding{JobID: r.JobID, ManagerID: manager.ID}
		s.Bindings = append(s.Bindings, b)
		if err = c.communicationSave(s); err != nil {
			return CommunicationResult{}, err
		}
		return CommunicationResult{Binding: &b}, nil
	case "release":
		if manager == nil {
			return CommunicationResult{}, actionError(1, "manager_lease_required", "active manager lease required", false, "", "")
		}
		for i, b := range s.Bindings {
			if b.JobID == r.JobID && b.ManagerID == manager.ID {
				for _, m := range s.Messages {
					if m.Source.ID == r.JobID && communicationMessageBlocksRelease(m) {
						return CommunicationResult{}, actionError(1, "requests_outstanding", "worker has unresolved requests or uncertain replies", false, r.JobID, "")
					}
				}
				s.Bindings = append(s.Bindings[:i], s.Bindings[i+1:]...)
				if err = c.communicationSave(s); err != nil {
					return CommunicationResult{}, err
				}
				return CommunicationResult{Released: true}, nil
			}
		}
		return CommunicationResult{}, actionError(1, "binding_unknown", "worker is not assigned to this manager", false, r.JobID, "")
	case "start":
		if manager == nil || !sameSessionCaller(*manager, *r.Caller) {
			return CommunicationResult{}, actionError(1, "manager_lease_required", "active manager lease required", false, "", "")
		}
		if validateName(r.Name) != nil || r.Cwd == "" || r.Task == "" {
			return CommunicationResult{}, actionError(2, "invalid_input", "invalid managed start", false, "", r.Name)
		}
		expectedManager := *manager
		leaseCopy := *manager.Lease
		expectedManager.Lease = &leaseCopy
		unlock()
		locked = false
		facts, e := c.startManaged(ctx, r.Name, r.Cwd, []byte(r.Task), expectedManager)
		if e != nil {
			return CommunicationResult{}, e
		}
		return CommunicationResult{Started: &facts}, nil
	case "publish":
		if r.Caller.JobID == "" || !idPattern.MatchString(r.MessageID) || !utf8.ValidString(r.Text) || len(r.Text) == 0 || len(r.Text) > MaxPublicationBytes || strings.TrimSpace(r.Text) == "" || (r.Kind != "question" && r.Kind != "blocker" && r.Kind != "result") {
			return CommunicationResult{}, actionError(2, "invalid_message", "invalid communication message", false, "", "")
		}
		target, e := c.ControlTarget(ctx, r.Caller.JobID)
		if e != nil || target.SessionID != r.Caller.SessionID || target.Generation != r.Caller.Generation || target.Pane.PID != r.Caller.PID {
			return CommunicationResult{}, actionError(1, "caller_mismatch", "worker controller identity mismatch", true, r.Caller.JobID, "")
		}
		bi := -1
		for i, b := range s.Bindings {
			if b.JobID == target.ID {
				bi = i
			}
		}
		if bi < 0 {
			return CommunicationResult{}, actionError(1, "manager_unavailable", "worker has no assigned manager", false, target.ID, "")
		}
		var managerID = s.Bindings[bi].ManagerID
		var manager *CommunicationManager
		for i := range s.Managers {
			if s.Managers[i].ID == managerID {
				manager = &s.Managers[i]
			}
		}
		if manager == nil {
			return CommunicationResult{}, errors.New("bound manager missing")
		}
		for _, m := range s.Messages {
			if m.MessageID == r.MessageID {
				if m.ManagerID == managerID && sameTarget(m.Source, target) && m.Kind == r.Kind && m.Text == r.Text {
					return CommunicationResult{Message: &m}, nil
				}
				return CommunicationResult{}, actionError(1, "message_conflict", "message ID conflicts with retained message", false, target.ID, "")
			}
		}
		pending := 0
		workerPending := 0
		for _, m := range s.Messages {
			if m.ManagerID == managerID && m.Source.ID == target.ID && m.Kind != "result" && m.ResolvedAt == 0 {
				workerPending++
			}
			if m.ManagerID == managerID && m.Kind != "result" && m.ResolvedAt == 0 {
				pending++
			}
		}
		if workerPending >= 16 || pending >= 64 {
			s.Degraded = true
			if saveErr := c.communicationSave(s); saveErr != nil {
				return CommunicationResult{}, saveErr
			}
			return CommunicationResult{}, actionError(1, "queue_full", "communication queue is full", false, target.ID, "")
		}
		m := CommunicationMessage{MessageID: r.MessageID, ManagerID: managerID, Source: target, Kind: r.Kind, Text: r.Text, CreatedAt: time.Now().UnixMilli()}
		candidate := s
		if !reserveCommunicationMessage(&candidate, m, time.Now()) {
			if saveErr := c.communicationSave(candidate); saveErr != nil {
				return CommunicationResult{}, saveErr
			}
			return CommunicationResult{}, actionError(1, "queue_full", "communication storage is full", false, target.ID, "")
		}
		if err = c.communicationSave(candidate); err != nil {
			return CommunicationResult{}, err
		}
		return CommunicationResult{Message: &m}, nil
	default:
		return CommunicationResult{}, actionError(2, "invalid_action", "unsupported communication action", false, "", "")
	}
}
func validateCommunicationText(text string) error {
	if !utf8.ValidString(text) || len(text) == 0 || len(text) > MaxPublicationBytes || strings.TrimSpace(text) == "" {
		return errors.New("invalid communication text")
	}
	return nil
}
func validateCommunicationNote(note string) error { return validateCommunicationText(note) }
func validManagerConversation(c CommunicationCaller) error {
	p, e := filepath.Abs(c.SessionFile)
	resolved, resolveErr := filepath.EvalSymlinks(c.SessionFile)
	if e != nil || resolveErr != nil || p != c.SessionFile || resolved != c.SessionFile || !idPattern.MatchString(c.Generation) {
		return errors.New("noncanonical path")
	}
	// Pi creates sessions with the current umask; identity requires integrity, not private-record mode.
	fd, e := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if e != nil {
		return errors.New("session file cannot be verified")
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	var info unix.Stat_t
	if e := unix.Fstat(fd, &info); e != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || info.Nlink != 1 || info.Mode&0022 != 0 {
		return errors.New("session file is not integrity-safe and current-owner")
	}
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(f, 4096)).Decode(&header) != nil || header.Type != "session" || header.ID != c.SessionID || c.SessionID == "" {
		return errors.New("session header mismatch")
	}
	return nil
}
func sameSessionCaller(m CommunicationManager, c CommunicationCaller) bool {
	return m.SessionFile == c.SessionFile && m.SessionID == c.SessionID && m.Lease != nil && m.Lease.Generation == c.Generation
}
func communicationCounts(ms []CommunicationMessage) (int, int) {
	p, e := 0, 0
	for _, m := range ms {
		pending := communicationMessageBlocksForget(m)
		if pending {
			p++
		}
		if m.Kind != "result" && m.ResolvedAt == 0 && m.EscalatedAt != 0 {
			e++
		}
	}
	return p, e
}
func communicationPage(ms []CommunicationMessage, cursor int) ([]CommunicationMessage, *int) {
	if cursor > len(ms) {
		cursor = len(ms)
	}
	end := cursor + CommunicationPageSize
	if end > len(ms) {
		end = len(ms)
	}
	out := append([]CommunicationMessage{}, ms[cursor:end]...)
	if end < len(ms) {
		return out, &end
	}
	return out, nil
}
func messageFingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
