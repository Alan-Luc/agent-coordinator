package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommunicationStateRejectsUnknownSchemaWithoutRewrite(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "communication")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	original := []byte(`{"schema":99,"protocol":1,"managers":[],"bindings":[],"messages":[]}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	c := Coordinator{store: s}
	if _, err := c.communicationLoad(); err == nil {
		t.Fatal("unknown schema was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("unknown schema document was rewritten")
	}
}

func TestCommunicationDocumentUsesOneMiBBound(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := Coordinator{store: s}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	managerID := "123e4567-e89b-42d3-a456-426614174000"
	target := SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174001", SessionID: "worker", Generation: "123e4567-e89b-42d3-a456-426614174002", Pane: identity}
	message := CommunicationMessage{MessageID: "123e4567-e89b-42d3-a456-426614174003", ManagerID: managerID, Source: target, Kind: "question", Text: "request", CreatedAt: time.Now().UnixMilli()}
	for i := 0; i < 6000; i++ {
		message.Replies = append(message.Replies, CommunicationReply{RequestID: fmt.Sprintf("123e4567-e89b-42d3-a456-%012x", i), Fingerprint: strings.Repeat("a", 64), Target: target})
	}
	st := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{{ID: managerID, SessionFile: "/tmp/session", SessionID: "manager"}}, Bindings: []CommunicationBinding{}, Messages: []CommunicationMessage{message}, Seen: []communicationSeen{}}
	if err := c.communicationSave(st); err == nil {
		t.Fatal("oversized communication document was persisted")
	}
}

func TestCommunicationPageEmptyIsNonNilJSONArray(t *testing.T) {
	page, next := communicationPage(nil, 0)
	if page == nil || next != nil {
		t.Fatalf("empty page=%#v cursor=%v", page, next)
	}
	encoded, err := json.Marshal(page)
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("empty page JSON=%s, err=%v", encoded, err)
	}
}

func TestCommunicationPageDoesNotSkipUnseenMessages(t *testing.T) {
	messages := make([]CommunicationMessage, CommunicationPageSize+1)
	for i := range messages {
		messages[i].MessageID = string(rune('a' + i))
	}
	page, next := communicationPage(messages, 0)
	if len(page) != CommunicationPageSize || next == nil || *next != CommunicationPageSize {
		t.Fatalf("first page=%d cursor=%v", len(page), next)
	}
	last, end := communicationPage(messages, *next)
	if len(last) != 1 || end != nil || last[0].MessageID != messages[CommunicationPageSize].MessageID {
		t.Fatal("next page skipped retained message")
	}
}

func TestDiagnosticsReadTheCoordinatorStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "coordinator-state")
	store, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	managerID := "123e4567-e89b-42d3-a456-426614174000"
	jobID := "123e4567-e89b-42d3-a456-426614174001"
	generation := "123e4567-e89b-42d3-a456-426614174002"
	state := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{{ID: managerID, SessionFile: "/tmp/session.jsonl", SessionID: "session", Enabled: true}}, Bindings: []CommunicationBinding{{JobID: jobID, ManagerID: managerID}}, Messages: []CommunicationMessage{{MessageID: "123e4567-e89b-42d3-a456-426614174003", ManagerID: managerID, Source: SessionTarget{ID: jobID, SessionID: "worker", Generation: generation, Pane: identity}, Kind: "question", Text: "answer?", CreatedAt: time.Now().UnixMilli()}}, Seen: []communicationSeen{}}
	c := &Coordinator{store: store}
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_STATE_DIR", filepath.Join(t.TempDir(), "different-default"))
	got, err := c.CommunicationMessages(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 || got.Messages[0].MessageID != state.Messages[0].MessageID {
		t.Fatalf("diagnostics read wrong state: %+v", got)
	}
}

func TestReconnectPreservesPausedOptIn(t *testing.T) {
	c, caller := communicationFixture(t)
	parentPID := os.Getppid()
	for _, action := range []string{"enroll", "pause", "reconnect"} {
		if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: action, Caller: &caller}, parentPID); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	state, err := c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	if state.Managers[0].Enabled {
		t.Fatal("reconnect silently re-enabled explicitly paused manager")
	}
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "inbox", Caller: &caller}, parentPID); err == nil {
		t.Fatal("paused manager received inbox delivery")
	}
}

func TestManagerOperationRejectsChangedPersistedConversationHeader(t *testing.T) {
	c, caller := communicationFixture(t)
	parentPID := os.Getppid()
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "enroll", Caller: &caller}, parentPID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caller.SessionFile, []byte(`{"type":"session","id":"replacement"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "inbox", Caller: &caller}, parentPID); err == nil {
		t.Fatal("manager operation accepted a changed persisted conversation header")
	}
}

func TestSameGenerationWithDifferentBirthCannotPauseOrDisconnect(t *testing.T) {
	c, caller := communicationFixture(t)
	parentPID := os.Getppid()
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "enroll", Caller: &caller}, parentPID); err != nil {
		t.Fatal(err)
	}
	state, err := c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	state.Managers[0].Lease.Process.StartSec++
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"pause", "disconnect", "reconnect", "enroll"} {
		if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: action, Caller: &caller}, parentPID); err == nil {
			t.Fatalf("%s accepted same generation with a different process birth", action)
		}
	}
}

func TestExplicitEnrollReenablesSameConversation(t *testing.T) {
	c, caller := communicationFixture(t)
	parentPID := os.Getppid()
	for _, action := range []string{"enroll", "pause", "enroll"} {
		if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: action, Caller: &caller}, parentPID); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	state, err := c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Managers) != 1 || !state.Managers[0].Enabled {
		t.Fatalf("explicit enrollment failed to re-enable exact conversation: %+v", state.Managers)
	}
}

func communicationFixture(t *testing.T) (*Coordinator, CommunicationCaller) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	store, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "test-session"
	sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"type":"session","id":"`+sessionID+`"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionFile, err = filepath.EvalSymlinks(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	return &Coordinator{store: store}, CommunicationCaller{SessionFile: sessionFile, SessionID: sessionID, Generation: "123e4567-e89b-42d3-a456-426614174002", PID: os.Getppid()}
}

func TestRetentionNeverEvictsProtectedMessages(t *testing.T) {
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour).UnixMilli()
	state := communicationState{Messages: []CommunicationMessage{
		{MessageID: "unresolved", Kind: "question", CreatedAt: old},
		{MessageID: "resolved", Kind: "question", CreatedAt: old, ResolvedAt: old + 1},
		{MessageID: "acked-result", Kind: "result", CreatedAt: old, AcknowledgedAt: old + 1},
		{MessageID: "unacked-result", Kind: "result", CreatedAt: old},
		{MessageID: "resolved-unacked-result", Kind: "result", CreatedAt: old, ResolvedAt: old + 1},
		{MessageID: "recently-resolved", Kind: "question", CreatedAt: old, ResolvedAt: now.Add(-time.Hour).UnixMilli()},
		{MessageID: "uncertain-reply", Kind: "question", CreatedAt: old, ResolvedAt: old + 1, Replies: []CommunicationReply{{RequestID: "123e4567-e89b-42d3-a456-426614174000"}}},
	}}
	if !pruneExpiredCommunicationMessages(&state, now) {
		t.Fatal("expired eligible records were not pruned")
	}
	if len(state.Messages) != 4 || state.Messages[0].MessageID != "unresolved" || state.Messages[1].MessageID != "unacked-result" || state.Messages[2].MessageID != "resolved-unacked-result" || state.Messages[3].MessageID != "recently-resolved" {
		t.Fatalf("retention evicted protected records: %+v", state.Messages)
	}
}

func TestUnresolvedReplyClaimBlocksForgetAndCountsPending(t *testing.T) {
	message := CommunicationMessage{MessageID: "123e4567-e89b-42d3-a456-426614174000", Kind: "question", Replies: []CommunicationReply{{RequestID: "123e4567-e89b-42d3-a456-426614174001"}}}
	if !communicationMessageBlocksForget(message) {
		t.Fatal("Forget ignored an incomplete reply claim")
	}
	if !communicationMessageBlocksRelease(message) {
		t.Fatal("release ignored an incomplete reply claim")
	}
	if pending, _ := communicationCounts([]CommunicationMessage{message}); pending != 1 {
		t.Fatalf("uncertain reply not visible as pending: %d", pending)
	}
}

func TestPressureEvictsEligibleOnlyAndPersistsDegradedWhenProtected(t *testing.T) {
	state := communicationState{Messages: make([]CommunicationMessage, 128)}
	for i := range state.Messages {
		state.Messages[i] = CommunicationMessage{MessageID: string(rune('a' + i)), Kind: "question", CreatedAt: int64(i + 1)}
	}
	state.Messages[0].ResolvedAt = 2
	if !reserveCommunicationMessage(&state, CommunicationMessage{MessageID: "new", Kind: "question", Text: "pending"}, time.Now()) {
		t.Fatal("eligible resolved record did not make room")
	}
	if len(state.Messages) != 128 || state.Messages[len(state.Messages)-1].MessageID != "new" {
		t.Fatal("pressure did not evict only the eligible record")
	}
	protected := communicationState{Messages: make([]CommunicationMessage, 128)}
	for i := range protected.Messages {
		protected.Messages[i] = CommunicationMessage{MessageID: string(rune('a' + i)), Kind: "question", CreatedAt: int64(i + 1)}
	}
	if reserveCommunicationMessage(&protected, CommunicationMessage{MessageID: "new", Kind: "question", Text: "pending"}, time.Now()) {
		t.Fatal("queue accepted work without eviction capacity")
	}
	if !protected.Degraded || len(protected.Messages) != 128 || protected.Messages[0].MessageID != "a" {
		t.Fatal("pressure was not visible or evicted protected requests")
	}
}

func TestManagedStartRefusesFullBindingQueueBeforePublishingJob(t *testing.T) {
	if _, err := currentBoot(); err != nil {
		t.Skip("managed start requires native boot identity")
	}
	store, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	manager := CommunicationManager{ID: "123e4567-e89b-42d3-a456-426614174000", SessionFile: "/tmp/manager", SessionID: "manager", Enabled: true, Lease: &CommunicationLease{Generation: "123e4567-e89b-42d3-a456-426614174001", Process: identity}}
	state := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{manager}, Bindings: make([]CommunicationBinding, 128), Messages: []CommunicationMessage{}, Seen: []communicationSeen{}}
	for i := range state.Bindings {
		state.Bindings[i] = CommunicationBinding{JobID: fmt.Sprintf("223e4567-e89b-42d3-a456-%012x", i), ManagerID: manager.ID}
	}
	c := &Coordinator{store: store}
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	tmuxPath := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nprintf 'tmux 3.5\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.options = Options{PiPath: "/usr/bin/true", TmuxPath: tmuxPath, Executable: "/usr/bin/true"}
	_, err = c.startManaged(context.Background(), "unbound-start", t.TempDir(), []byte("task"), manager)
	var action *ActionError
	if !errors.As(err, &action) || action.Diagnostic.Code != "queue_full" {
		t.Fatalf("binding capacity did not refuse start: %v", err)
	}
	jobs, err := os.ReadDir(filepath.Join(store.root, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("rejected managed start left a job reservation: %v", jobs)
	}
}

func TestCommunicationStatusSanitizesAndBoundsUntrustedText(t *testing.T) {
	attention := "line\x1b[31mred\x07"
	status := sanitizeCommunicationStatus(SessionStatus{SessionID: "worker", Report: &AgentReport{Summary: strings.Repeat("x", 1024), Attention: &attention}, Prompt: &ObservedPrompt{Kind: "question", Title: "native"}})
	if strings.Contains(*status.Report.Attention, "\x1b") || strings.Contains(*status.Report.Attention, "\x07") {
		t.Fatal("terminal controls survived status sanitization")
	}
	if len(status.Report.Summary) > 256 || len(*status.Report.Attention) > 256 || len(status.Prompt.Title) > 256 {
		t.Fatal("communication status exceeded response text bound")
	}
}

func TestSeenPageIsFencedToManagerRuntime(t *testing.T) {
	state := communicationState{Seen: []communicationSeen{{ManagerID: "123e4567-e89b-42d3-a456-426614174000", MessageID: "123e4567-e89b-42d3-a456-426614174001", Generation: "123e4567-e89b-42d3-a456-426614174002", SeenAt: 1}}}
	if !communicationMessageSeen(state, state.Seen[0].ManagerID, state.Seen[0].Generation, state.Seen[0].MessageID) {
		t.Fatal("current runtime lost seen marker")
	}
	if communicationMessageSeen(state, state.Seen[0].ManagerID, "123e4567-e89b-42d3-a456-426614174003", state.Seen[0].MessageID) {
		t.Fatal("new runtime inherited a stale seen marker")
	}
}

func TestAttemptClaimIsPerRuntimeAndNeverResendsWithinRuntime(t *testing.T) {
	messages := []CommunicationMessage{{MessageID: "123e4567-e89b-42d3-a456-426614174000", ManagerID: "123e4567-e89b-42d3-a456-426614174001", Kind: "question"}}
	first, changed := claimCommunicationAttempts(messages, messages[0].ManagerID, "123e4567-e89b-42d3-a456-426614174002", []string{messages[0].MessageID})
	if !changed || len(first) != 1 {
		t.Fatalf("first runtime claim=%+v changed=%v", first, changed)
	}
	again, changed := claimCommunicationAttempts(messages, messages[0].ManagerID, "123e4567-e89b-42d3-a456-426614174002", []string{messages[0].MessageID})
	if changed || len(again) != 0 {
		t.Fatalf("same runtime retried attempt: %+v", again)
	}
	reconnect, changed := claimCommunicationAttempts(messages, messages[0].ManagerID, "123e4567-e89b-42d3-a456-426614174003", []string{messages[0].MessageID})
	if !changed || len(reconnect) != 1 {
		t.Fatalf("new runtime could not replay unacknowledged message: %+v", reconnect)
	}
}

func TestReplyCannotTargetResolvedOrResultMessages(t *testing.T) {
	for _, message := range []CommunicationMessage{{Kind: "result"}, {Kind: "question", ResolvedAt: time.Now().UnixMilli()}} {
		t.Run(message.Kind, func(t *testing.T) {
			c, caller := communicationFixture(t)
			parentPID := os.Getppid()
			if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "enroll", Caller: &caller}, parentPID); err != nil {
				t.Fatal(err)
			}
			state, err := c.communicationLoad()
			if err != nil {
				t.Fatal(err)
			}
			identity, err := identifyProcess(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			message.MessageID = "123e4567-e89b-42d3-a456-426614174003"
			message.ManagerID = state.Managers[0].ID
			message.Source = SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174004", SessionID: "worker", Generation: "123e4567-e89b-42d3-a456-426614174005", Pane: identity}
			message.Text = "request"
			message.CreatedAt = time.Now().UnixMilli()
			state.Messages = []CommunicationMessage{message}
			if err := c.communicationSave(state); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "inbox", Caller: &caller}, parentPID); err != nil {
				t.Fatal(err)
			}
			_, err = c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "reply", Caller: &caller, MessageID: message.MessageID, RequestID: "123e4567-e89b-42d3-a456-426614174006", Text: "answer"}, parentPID)
			action, ok := err.(*ActionError)
			if !ok || action.Diagnostic.Code != "reply_not_allowed" {
				t.Fatalf("reply error=%v, want reply_not_allowed", err)
			}
		})
	}
}

func TestCompletedReplyReceiptSurvivesResolution(t *testing.T) {
	c, caller := communicationFixture(t)
	parentPID := os.Getppid()
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "enroll", Caller: &caller}, parentPID); err != nil {
		t.Fatal(err)
	}
	state, err := c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	target := SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174004", SessionID: "worker", Generation: "123e4567-e89b-42d3-a456-426614174005", Pane: identity}
	requestID, text := "123e4567-e89b-42d3-a456-426614174006", "answer"
	receipt := ControlReceipt{ID: target.ID, SessionID: target.SessionID, Generation: target.Generation, RequestID: requestID, Operation: "send", Result: "api_invoked"}
	state.Messages = []CommunicationMessage{{MessageID: "123e4567-e89b-42d3-a456-426614174003", ManagerID: state.Managers[0].ID, Source: target, Kind: "question", Text: "request", CreatedAt: time.Now().UnixMilli(), ResolvedAt: time.Now().UnixMilli(), Replies: []CommunicationReply{{RequestID: requestID, Fingerprint: messageFingerprint(text), Target: target, Receipt: &receipt}}}}
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "inbox", Caller: &caller}, parentPID); err != nil {
		t.Fatal(err)
	}
	result, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "reply", Caller: &caller, MessageID: state.Messages[0].MessageID, RequestID: requestID, Text: text}, parentPID)
	if err != nil || result.Receipt == nil || result.Receipt.RequestID != requestID {
		t.Fatalf("completed duplicate receipt=%+v err=%v", result.Receipt, err)
	}
}

func TestReplyClaimsAreIdempotentAndConflictsAreRefused(t *testing.T) {
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	target := SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174000", SessionID: "session", Generation: "123e4567-e89b-42d3-a456-426614174001", Pane: identity}
	message := CommunicationMessage{Source: target}
	requestID := "123e4567-e89b-42d3-a456-426614174002"
	if receipt, err := prepareCommunicationReply(&message, requestID, "reply", target); err != nil || receipt != nil || len(message.Replies) != 1 {
		t.Fatalf("claim: receipt=%v err=%v replies=%d", receipt, err, len(message.Replies))
	}
	if _, err := prepareCommunicationReply(&message, requestID, "reply", target); err == nil {
		t.Fatal("incomplete duplicate reply was treated as safe to retry")
	}
	if _, err := prepareCommunicationReply(&message, requestID, "different", target); err == nil {
		t.Fatal("conflicting duplicate reply was accepted")
	}
	message.Replies[0].Receipt = &ControlReceipt{ID: target.ID, SessionID: target.SessionID, Generation: target.Generation, RequestID: requestID, Operation: "send", Result: "api_invoked"}
	receipt, err := prepareCommunicationReply(&message, requestID, "reply", target)
	if err != nil || receipt == nil || receipt.RequestID != requestID {
		t.Fatalf("completed duplicate receipt=%+v err=%v", receipt, err)
	}
}

func TestReaperRemovesOnlyOwnedCommunicationTemps(t *testing.T) {
	c, _ := communicationFixture(t)
	dir := filepath.Join(c.store.root, "communication")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, ".write-orphan")
	if err := os.WriteFile(temp, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := c.store.lock(context.Background(), "registry.lock")
	if err != nil {
		t.Fatal(err)
	}
	err = c.reapCommunicationTemps()
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(temp); !os.IsNotExist(err) {
		t.Fatalf("owned temp remains: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}
	state, err := c.communicationLoad()
	if err != nil || len(state.Messages) != 0 {
		t.Fatalf("empty communication directory did not load as empty: state=%+v err=%v", state, err)
	}
}

func TestAcknowledgementDoesNotResolveRequest(t *testing.T) {
	c, caller := communicationFixture(t)
	parentPID := os.Getppid()
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "enroll", Caller: &caller}, parentPID); err != nil {
		t.Fatal(err)
	}
	state, err := c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	manager := state.Managers[0]
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	message := CommunicationMessage{MessageID: "123e4567-e89b-42d3-a456-426614174003", ManagerID: manager.ID, Source: SessionTarget{ID: "123e4567-e89b-42d3-a456-426614174004", SessionID: "worker", Generation: "123e4567-e89b-42d3-a456-426614174005", Pane: identity}, Kind: "question", Text: "answer?", CreatedAt: time.Now().UnixMilli()}
	state.Messages = []CommunicationMessage{message}
	state.Seen = []communicationSeen{}
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	ack := CommunicationRequest{Protocol: CommunicationProtocol, Action: "ack", Caller: &caller, MessageIDs: []string{message.MessageID}}
	if _, err := c.Communication(context.Background(), ack, parentPID); err == nil {
		t.Fatal("unseen message was acknowledged")
	}
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: CommunicationProtocol, Action: "inbox", Caller: &caller}, parentPID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Communication(context.Background(), ack, parentPID); err != nil {
		t.Fatal(err)
	}
	state, err = c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	if state.Messages[0].AcknowledgedAt == 0 || state.Messages[0].ResolvedAt != 0 {
		t.Fatalf("ack changed request resolution: %+v", state.Messages[0])
	}
	pending, _ := communicationCounts(state.Messages)
	if pending != 1 {
		t.Fatalf("ack cleared outstanding request count: %d", pending)
	}
}

func TestDegradedPressureStateIsPersistedAndVisible(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: store}
	state := communicationState{Schema: 1, Protocol: CommunicationProtocol, Managers: []CommunicationManager{}, Bindings: []CommunicationBinding{}, Messages: []CommunicationMessage{}, Seen: []communicationSeen{}, Degraded: true}
	if err := c.communicationSave(state); err != nil {
		t.Fatal(err)
	}
	got, err := c.CommunicationMessages(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Degraded {
		t.Fatal("persistent queue pressure was not visible in diagnostics")
	}
}

func TestAbsentDiagnosticsDoNotCreateCommunicationDocument(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{store: store}
	got, err := c.CommunicationMessages(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 0 || got.Pending != 0 {
		t.Fatalf("unexpected diagnostics: %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(store.root, "communication")); !os.IsNotExist(err) {
		t.Fatalf("diagnostics created communication document directory: %v", err)
	}
}
