package coordinator

import (
	"errors"
	"time"
)

func communicationMessageSeen(state communicationState, managerID, generation, messageID string) bool {
	for _, entry := range state.Seen {
		if entry.ManagerID == managerID && entry.Generation == generation && entry.MessageID == messageID {
			return true
		}
	}
	return false
}

func markCommunicationMessagesSeen(state *communicationState, managerID, generation string, messages []CommunicationMessage, now time.Time) error {
	for _, message := range messages {
		found := false
		for i := range state.Seen {
			entry := &state.Seen[i]
			if entry.ManagerID == managerID && entry.MessageID == message.MessageID {
				if entry.Generation != generation {
					entry.Generation, entry.SeenAt = generation, now.UnixMilli()
				}
				found = true
				break
			}
		}
		if !found {
			state.Seen = append(state.Seen, communicationSeen{ManagerID: managerID, MessageID: message.MessageID, Generation: generation, SeenAt: now.UnixMilli()})
		}
	}
	if len(state.Seen) > 2048 {
		return errors.New("communication seen-page capacity full")
	}
	return nil
}

func claimCommunicationAttempts(messages []CommunicationMessage, managerID, generation string, ids []string) ([]CommunicationMessage, bool) {
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	claimed := make([]CommunicationMessage, 0, len(selected))
	changed := false
	for index := range messages {
		message := &messages[index]
		if !selected[message.MessageID] || message.ManagerID != managerID || message.ResolvedAt != 0 || message.AcknowledgedAt != 0 || message.AttemptGeneration == generation {
			continue
		}
		message.AttemptGeneration = generation
		claimed = append(claimed, *message)
		changed = true
	}
	return claimed, changed
}

func prepareCommunicationReply(message *CommunicationMessage, requestID, text string, target SessionTarget) (*ControlReceipt, error) {
	if !idPattern.MatchString(requestID) || validateCommunicationText(text) != nil {
		return nil, actionError(2, "invalid_reply", "reply requires a UUID and valid text", false, target.ID, "")
	}
	fingerprint := messageFingerprint(text)
	for _, saved := range message.Replies {
		if saved.RequestID != requestID {
			continue
		}
		if saved.Fingerprint != fingerprint || !sameTarget(saved.Target, target) {
			return nil, actionError(1, "reply_conflict", "request ID conflicts with saved reply", false, target.ID, "")
		}
		if saved.Receipt != nil {
			return saved.Receipt, nil
		}
		if saved.Failure != nil {
			return nil, &ActionError{ExitCode: 1, Diagnostic: *saved.Failure}
		}
		return nil, actionError(75, "reply_uncertain", "reply claim is incomplete; do not retry", true, target.ID, "")
	}
	message.Replies = append(message.Replies, CommunicationReply{RequestID: requestID, Fingerprint: fingerprint, Target: target})
	return nil, nil
}
