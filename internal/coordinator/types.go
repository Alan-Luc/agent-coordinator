package coordinator

import (
	"errors"
	"time"
)

const SchemaVersion = 1
const ProtocolVersion = "1"
const BootIdentityScheme = "darwin-boot-session-uuid"
const MaxTaskBytes = 1 << 20
const TaskHeader = "Initial task supplied by agent-coordinator:\n\n"

var ErrProcessGone = errors.New("process ended")

type ProcessIdentity struct {
	BootID    string `json:"boot_id"`
	PID       int    `json:"pid"`
	UID       uint32 `json:"uid"`
	StartSec  int64  `json:"start_sec"`
	StartUsec int64  `json:"start_usec"`
}

type ProcessStatus string

const (
	ProcessAlive   ProcessStatus = "alive"
	ProcessEnded   ProcessStatus = "ended"
	ProcessUnknown ProcessStatus = "unknown"
)

type Manifest struct {
	Schema       int       `json:"schema"`
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Owner        string    `json:"owner"`
	Cwd          string    `json:"cwd"`
	Pi           string    `json:"pi"`
	Tmux         string    `json:"tmux"`
	Coordinator  string    `json:"coordinator"`
	Created      time.Time `json:"created"`
	BootID       string    `json:"boot_id"`
	BootIDScheme string    `json:"boot_id_scheme,omitempty"`
	Protocol     string    `json:"protocol"`
	Socket       string    `json:"socket"`
}

type Launch struct {
	Schema  int              `json:"schema"`
	Phase   string           `json:"phase"`
	Server  *ProcessIdentity `json:"server,omitempty"`
	Pane    *ProcessIdentity `json:"pane,omitempty"`
	Error   string           `json:"error,omitempty"`
	Retired bool             `json:"retired,omitempty"`
}

// JobFacts describe observed process facts, never semantic task progress.
type JobFacts struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Launch      string         `json:"launch"`
	Terminal    string         `json:"terminal"`
	PiReadiness string         `json:"pi_readiness"`
	TaskOutcome string         `json:"task_outcome"`
	Clients     int            `json:"clients"`
	Retired     bool           `json:"retired"`
	NextAction  string         `json:"next_action"`
	ExitStatus  *int           `json:"exit_status,omitempty"`
	Session     *SessionStatus `json:"session,omitempty"`
}

const ControlProtocol = 1
const MaxControlTextBytes = 64 * 1024

type AgentReport struct {
	Summary   string  `json:"summary"`
	Attention *string `json:"attention,omitempty"`
}

type ObservedPrompt struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
}

type SessionStatus struct {
	Protocol   int             `json:"protocol"`
	JobID      string          `json:"job_id"`
	Owner      string          `json:"owner"`
	SessionID  string          `json:"session_id"`
	Generation string          `json:"generation"`
	PID        int             `json:"pid"`
	UpdatedAt  int64           `json:"updated_at"`
	Activity   string          `json:"activity"`
	Prompt     *ObservedPrompt `json:"prompt,omitempty"`
	Report     *AgentReport    `json:"report,omitempty"`
}

type SessionTarget struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id"`
	Generation string          `json:"generation"`
	Pane       ProcessIdentity `json:"pane"`
}

type ControlRequest struct {
	Protocol   int    `json:"protocol"`
	JobID      string `json:"job_id"`
	SessionID  string `json:"session_id"`
	Generation string `json:"generation"`
	RequestID  string `json:"request_id"`
	Action     string `json:"action"`
	Text       string `json:"text,omitempty"`
}

type ControlReceipt struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Generation string `json:"generation"`
	RequestID  string `json:"request_id"`
	Operation  string `json:"operation"`
	Result     string `json:"result"`
}

type StopAuthorization struct {
	Token     string        `json:"token"`
	Target    SessionTarget `json:"target"`
	Operation string        `json:"operation"`
	ExpiresAt int64         `json:"expires_at"`
}

type Diagnostic struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Uncertain bool   `json:"uncertain"`
}

type ListResult struct {
	Jobs []JobFacts `json:"jobs"`
}
type ErrorResult struct {
	Error Diagnostic `json:"error"`
}

type ActionError struct {
	ExitCode   int
	Diagnostic Diagnostic
}

func (e *ActionError) Error() string { return e.Diagnostic.Message }

type Options struct {
	StateDir   string
	PiPath     string
	TmuxPath   string
	Executable string
	Width      int
	Height     int
}
