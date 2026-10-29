package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (c *Coordinator) communicationWorkers(ctx context.Context, state communicationState, managerID string) []SessionStatus {
	workers := make([]SessionStatus, 0, len(state.Bindings))
	now := time.Now()
	for _, binding := range state.Bindings {
		if binding.ManagerID != managerID {
			continue
		}
		manifest, launch, err := c.store.load(binding.JobID)
		if err != nil || launch.Pane == nil {
			continue
		}
		target, err := c.ControlTarget(ctx, binding.JobID)
		if err != nil || target.Pane.PID != launch.Pane.PID {
			continue
		}
		status, err := readSessionStatus(filepath.Join(c.store.root, "jobs", binding.JobID, "session.json"), now)
		if err != nil || c.validateJobSessionStatus(manifest, launch, status, now) != nil || status.SessionID != target.SessionID || status.Generation != target.Generation {
			continue
		}
		workers = append(workers, sanitizeCommunicationStatus(status))
	}
	return workers
}

func sanitizeCommunicationStatus(status SessionStatus) SessionStatus {
	status.SessionID = sanitizeCommunicationDisplayText(status.SessionID, 256)
	if status.Prompt != nil {
		prompt := *status.Prompt
		prompt.Kind = sanitizeCommunicationDisplayText(prompt.Kind, 64)
		prompt.Title = sanitizeCommunicationDisplayText(prompt.Title, 256)
		status.Prompt = &prompt
	}
	if status.Report != nil {
		report := *status.Report
		report.Summary = sanitizeCommunicationDisplayText(report.Summary, 256)
		if report.Attention != nil {
			attention := sanitizeCommunicationDisplayText(*report.Attention, 256)
			report.Attention = &attention
		}
		status.Report = &report
	}
	return status
}

func sanitizeCommunicationDisplayText(text string, maxBytes int) string {
	var out strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) {
			r = ' '
		}
		if out.Len()+utf8.RuneLen(r) > maxBytes {
			break
		}
		out.WriteRune(r)
	}
	return strings.TrimSpace(out.String())
}
