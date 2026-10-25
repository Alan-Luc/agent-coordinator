package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seededCommunicationQuestion(t *testing.T) (*Coordinator, CommunicationCaller, communicationState) {
	t.Helper()
	c, caller := communicationFixture(t)
	if _, err := c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "enroll", Caller: &caller}, caller.PID); err != nil {
		t.Fatal(err)
	}
	s, err := c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	s.Messages = []CommunicationMessage{{MessageID: "223e4567-e89b-42d3-a456-426614174000", ManagerID: s.Managers[0].ID, Source: SessionTarget{ID: "323e4567-e89b-42d3-a456-426614174000", SessionID: "worker", Generation: caller.Generation, Pane: identity}, Kind: "question", Text: "Which fixture?", CreatedAt: time.Now().UnixMilli()}}
	return c, caller, s
}

func TestNativeSessionFilePermissionsPreserveIntegrity(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0644, 0660, 0666} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			c, caller := communicationFixture(t)
			if err := os.Chmod(caller.SessionFile, mode); err != nil {
				t.Fatal(err)
			}
			_, err := c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "enroll", Caller: &caller}, caller.PID)
			if mode&0022 == 0 && err != nil {
				t.Fatalf("native Pi's non-writable session mode refused: %v", err)
			}
			if mode&0022 != 0 && err == nil {
				t.Fatal("group/world-writable identity file accepted")
			}
			info, statErr := os.Stat(caller.SessionFile)
			if statErr != nil || info.Mode().Perm() != mode {
				t.Fatal("native file permissions were modified")
			}
		})
	}
}

func TestCommunicationWriteFailurePreservesUncertainty(t *testing.T) {
	c, _, state := seededCommunicationQuestion(t)
	path := filepath.Join(c.store.root, "communication", "state.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	err := c.communicationSave(state)
	var action *ActionError
	if !errors.As(err, &action) || !action.Diagnostic.Uncertain {
		t.Fatalf("write-path failure was reported as definite: %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "untouched" {
		t.Fatal("failed write changed the symlink target")
	}
}

func TestManagedStartPersistsAssociationBeforeDispatch(t *testing.T) {
	c, _, s := seededCommunicationQuestion(t)
	if err := c.communicationSave(s); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "dispatch")
	tmux := filepath.Join(t.TempDir(), "tmux")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = '-V' ]; then printf 'tmux 3.5\\n'; else printf dispatched > '%s'; fi\n", marker)
	if err := os.WriteFile(tmux, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(tmux, "-V").Output(); err != nil || string(output) != "tmux 3.5\n" {
		t.Fatalf("tmux fixture prerequisite failed: %q %v", output, err)
	}
	c.options = Options{PiPath: "/usr/bin/true", TmuxPath: tmux, Executable: "/usr/bin/true"}
	t.Cleanup(func() {
		ids, err := c.store.ids()
		if err != nil {
			t.Error(err)
			return
		}
		for _, id := range ids {
			manifest, _, err := c.store.load(id)
			if err != nil {
				t.Error(err)
				continue
			}
			if err := os.Remove(filepath.Dir(manifest.Socket)); err != nil {
				t.Error(err)
				continue
			}
			if err := os.Remove(filepath.Dir(filepath.Dir(manifest.Socket))); err != nil {
				t.Error(err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.startManaged(ctx, "associated", t.TempDir(), []byte("Inspect only"), s.Managers[0])
	var action *ActionError
	if !errors.As(err, &action) || action.Diagnostic.Code != "uncertain_launch" {
		t.Fatalf("admitted start did not reach mocked downstream dispatch: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dispatch never invoked: %v", err)
	}
	saved, err := c.communicationLoad()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Bindings) != 1 || saved.Bindings[0].ManagerID != s.Managers[0].ID || len(saved.Managers) != 1 || len(saved.Messages) != 1 {
		t.Fatalf("binding lost existing state: %+v", saved)
	}
}

func TestUnchangedInboxDoesNotRewriteCommunication(t *testing.T) {
	c, caller, s := seededCommunicationQuestion(t)
	if err := c.communicationSave(s); err != nil {
		t.Fatal(err)
	}
	request := CommunicationRequest{Protocol: 1, Action: "inbox", Caller: &caller}
	if _, err := c.Communication(context.Background(), request, caller.PID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.store.root, "communication", "state.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := c.Communication(context.Background(), request, caller.PID); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged inbox rewrote durable state")
	}
}

func TestAdmittedEscapedRequestsCanDrainAtCapacity(t *testing.T) {
	c, caller, s := seededCommunicationQuestion(t)
	template := s.Messages[0]
	s.Messages = []CommunicationMessage{}
	for i := 0; i < 64; i++ {
		m := template
		m.MessageID = fmt.Sprintf("223e4567-e89b-42d3-a456-%012x", i)
		m.Source.ID = fmt.Sprintf("323e4567-e89b-42d3-a456-%012x", i)
		m.Text = strings.Repeat("\x00", 4096)
		if !reserveCommunicationMessage(&s, m, time.Now()) {
			for size := 4095; size > 0; size-- {
				m.Text = strings.Repeat("\x00", size)
				if reserveCommunicationMessage(&s, m, time.Now()) {
					break
				}
			}
			break
		}
	}
	if len(s.Messages) < 2 {
		t.Fatal("capacity fixture admitted no useful queue")
	}
	if err := c.communicationSave(s); err != nil {
		t.Fatal(err)
	}
	inbox, err := c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "inbox", Caller: &caller}, caller.PID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "reply", Caller: &caller, MessageID: inbox.Inbox.Messages[0].MessageID, RequestID: "423e4567-e89b-42d3-a456-426614174000", Text: "Use the private fixture"}, caller.PID)
	var stale *ActionError
	if !errors.As(err, &stale) || stale.Diagnostic.Code != "identity_changed" {
		t.Fatalf("reply capacity prevented reaching the immutable target fence: %v", err)
	}
	for {
		current, err := c.communicationLoad()
		if err != nil {
			t.Fatal(err)
		}
		pending, _ := communicationCounts(current.Messages)
		if pending == 0 {
			break
		}
		var id string
		cursor := 0
		for id == "" {
			inbox, err := c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "inbox", Caller: &caller, Cursor: cursor}, caller.PID)
			if err != nil {
				t.Fatalf("accepted requests cannot be read at capacity: %v", err)
			}
			for _, m := range inbox.Inbox.Messages {
				if m.ResolvedAt == 0 {
					id = m.MessageID
					break
				}
			}
			if id != "" {
				break
			}
			if inbox.Inbox.NextCursor == nil {
				t.Fatal("inbox omitted unresolved requests")
			}
			cursor = *inbox.Inbox.NextCursor
		}
		if _, err = c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "attempt", Caller: &caller, MessageIDs: []string{id}}, caller.PID); err != nil {
			t.Fatal(err)
		}
		if _, err = c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "ack", Caller: &caller, MessageIDs: []string{id}}, caller.PID); err != nil {
			t.Fatal(err)
		}
		result, err := c.Communication(context.Background(), CommunicationRequest{Protocol: 1, Action: "resolve", Caller: &caller, MessageID: id, Note: strings.Repeat("\x00", 4096)}, caller.PID)
		if err != nil {
			t.Fatalf("accepted request cannot resolve at capacity: %v", err)
		}
		if result.Message.MessageID != id || result.Message.ResolvedAt == 0 {
			t.Fatal("pressure changed the resolution response identity")
		}
		bytes, err := os.ReadFile(filepath.Join(c.store.root, "communication", "state.json"))
		if err != nil || len(bytes) > MaxCommunicationBytes {
			t.Fatalf("storage bound violated: %d %v", len(bytes), err)
		}
	}
}

func TestExplicitResolutionClosesUncertainReply(t *testing.T) {
	m := CommunicationMessage{Kind: "question", ResolvedAt: 1, Replies: []CommunicationReply{{RequestID: "123e4567-e89b-42d3-a456-426614174000"}}}
	if communicationMessageBlocksForget(m) || communicationMessageBlocksRelease(m) || !eligibleCommunicationEviction(m) {
		t.Fatal("explicitly resolved request remained permanently pinned")
	}
	if pending, _ := communicationCounts([]CommunicationMessage{m}); pending != 0 {
		t.Fatal("resolved uncertain reply kept attention outstanding")
	}
}

func TestDefiniteReplyFailureRemainsIdempotent(t *testing.T) {
	_, _, s := seededCommunicationQuestion(t)
	m := s.Messages[0]
	id := "123e4567-e89b-42d3-a456-426614174001"
	m.Replies = []CommunicationReply{{RequestID: id, Fingerprint: messageFingerprint("answer"), Target: m.Source, Failure: &Diagnostic{Code: "rejected", Message: "control request was rejected", Uncertain: false}}}
	_, err := prepareCommunicationReply(&m, id, "answer", m.Source)
	var action *ActionError
	if !errors.As(err, &action) || action.Diagnostic.Code != "rejected" || action.Diagnostic.Uncertain {
		t.Fatalf("definite failure became ambiguous: %v", err)
	}
	if len(m.Replies) != 1 {
		t.Fatal("completed rejection created another dispatch claim")
	}
	encoded, err := json.Marshal(m)
	if err != nil || !strings.Contains(string(encoded), `"failure"`) {
		t.Fatal("definite rejection was not retained")
	}
}
