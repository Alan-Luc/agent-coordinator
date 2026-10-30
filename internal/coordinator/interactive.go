package coordinator

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"coordinate/internal/pilaunch"
	"coordinate/internal/tmux"
)

type InteractiveAttachResult struct {
	ID         string `json:"id"`
	Detached   bool   `json:"detached"`
	ExitStatus *int   `json:"exit_status,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Uncertain  bool   `json:"uncertain"`
}

type interactiveOutcome struct {
	Schema     int    `json:"schema"`
	ExitStatus int    `json:"exit_status"`
	Transcript string `json:"transcript"`
}

func (c *Coordinator) StartInteractive(ctx context.Context, cwd string, argv []string, native pilaunch.Managed) (JobFacts, error) {
	if argv == nil {
		argv = []string{}
	}
	if !filepath.IsAbs(native.Executable) || native.Observation.Version != "1.4.0" || pilaunch.Recheck(native) != nil {
		return JobFacts{}, actionError(2, "invalid_native_pi", "managed Pi observation is invalid or stale", false, "", "")
	}
	body, err := json.Marshal(LaunchArguments{Schema: SchemaVersion, Argv: argv})
	if err != nil || len(argv) > MaxLaunchArguments || len(body) > MaxLaunchArgumentBytes || !validArgumentJSONUnicode(body) {
		return JobFacts{}, actionError(2, "invalid_arguments", "interactive arguments exceed supported bounds", false, "", "")
	}
	for _, argument := range argv {
		if strings.ContainsRune(argument, 0) || !utf8.ValidString(argument) {
			return JobFacts{}, actionError(2, "invalid_arguments", "interactive arguments must be valid UTF-8 without NUL", false, "", "")
		}
	}
	return c.startInteractive(ctx, cwd, argv, native, body)
}

func (c *Coordinator) AttachInitialID(ctx context.Context, id string) (*exec.Cmd, error) {
	return c.attachProtocol2(ctx, id, "ac-"+id)
}

func sanitizeInteractiveOutput(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	var output strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		if output.Len()+utf8.RuneLen(r) > limit {
			break
		}
		output.WriteRune(r)
	}
	return output.String()
}

func (c *Coordinator) FinishInteractiveAttach(ctx context.Context, id string, waitErr error) (InteractiveAttachResult, error) {
	return c.finishInteractiveAttach(ctx, id, waitErr)
}

func (c *Coordinator) finishInteractiveAttach(ctx context.Context, id string, waitErr error) (InteractiveAttachResult, error) {
	m, launch, err := c.store.load(id)
	if err != nil || m.Protocol != InteractiveProtocolVersion || launch.Phase != "exec-intent" || launch.Server == nil || launch.Pane == nil {
		return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(1, "ownership_unknown", "interactive outcome could not be verified", true, id, "")
	}
	snap, err := c.ownedSnapshot(ctx, m)
	if err != nil {
		return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(1, "ownership_unknown", "interactive terminal could not be verified", true, id, "")
	}
	server, serverErr := identifyProcess(snap.ServerPID)
	if serverErr != nil || !sameIdentity(server, *launch.Server) {
		return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(1, "identity_changed", "interactive server identity changed", true, id, "")
	}
	if !snap.PaneDead {
		pane, paneErr := identifyProcess(snap.PanePID)
		if paneErr != nil || !sameIdentity(pane, *launch.Pane) {
			return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(1, "identity_changed", "interactive pane identity changed", true, id, "")
		}
		if waitErr != nil {
			return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(75, "attach_unconfirmed", "interactive client ended unexpectedly; runtime remains discoverable", true, id, "")
		}
		return InteractiveAttachResult{ID: id, Detached: true}, nil
	}
	paneStatus, paneErr := compareProcess(*launch.Pane)
	if paneErr != nil || paneStatus != ProcessEnded || snap.PanePID != launch.Pane.PID || snap.ExitStatus == nil {
		return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(75, "exit_unverified", "Pi exit status is not verified", true, id, "")
	}
	transcript, err := (tmux.Client{Executable: m.Tmux}).CaptureTail(ctx, m.Socket, snap.PaneID, 1000)
	if err != nil {
		return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(75, "capture_unavailable", "Pi exit diagnostics could not be captured", true, id, "")
	}
	transcript = sanitizeInteractiveOutput(transcript, 1<<20)
	if err := writeJSON(filepath.Join(c.store.root, "jobs", id), "interactive-outcome.json", interactiveOutcome{Schema: SchemaVersion, ExitStatus: *snap.ExitStatus, Transcript: transcript}); err != nil {
		return InteractiveAttachResult{ID: id, Uncertain: true}, actionError(75, "outcome_persist_failed", "Pi exit outcome could not be persisted", true, id, "")
	}
	if snap.Clients == 0 {
		if err := c.removeInteractiveResources(ctx, m, &launch, snap); err != nil {
			return InteractiveAttachResult{ID: id, ExitStatus: snap.ExitStatus, Transcript: transcript, Uncertain: true}, actionError(75, "cleanup_pending", "Pi exit was recorded; owned terminal cleanup remains uncertain", true, id, "")
		}
	}
	return InteractiveAttachResult{ID: id, ExitStatus: snap.ExitStatus, Transcript: transcript}, nil
}
