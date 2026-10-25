package coordinator

const CommunicationProtocol = 1
const MaxPublicationBytes = 4096
const MaxCommunicationBytes = 1 << 20
const MaxCommunicationRequestBytes = 6*MaxTaskBytes + 64*1024
const CommunicationPageSize = 8

type CommunicationCaller struct {
	SessionFile string `json:"session_file"`
	SessionID   string `json:"session_id"`
	Generation  string `json:"generation"`
	PID         int    `json:"pid"`
	JobID       string `json:"job_id,omitempty"`
}

type CommunicationLease struct {
	Generation string          `json:"generation"`
	Process    ProcessIdentity `json:"process"`
}

type CommunicationManager struct {
	ID          string              `json:"id"`
	SessionFile string              `json:"session_file"`
	SessionID   string              `json:"session_id"`
	Enabled     bool                `json:"enabled"`
	Lease       *CommunicationLease `json:"lease,omitempty"`
}

type CommunicationBinding struct {
	JobID     string `json:"job_id"`
	ManagerID string `json:"manager_id"`
}

type CommunicationReply struct {
	RequestID   string          `json:"request_id"`
	Fingerprint string          `json:"fingerprint"`
	Target      SessionTarget   `json:"target"`
	Receipt     *ControlReceipt `json:"receipt,omitempty"`
	Failure     *Diagnostic     `json:"failure,omitempty"`
}

type CommunicationMessage struct {
	MessageID         string               `json:"message_id"`
	ManagerID         string               `json:"manager_id"`
	Source            SessionTarget        `json:"source"`
	Kind              string               `json:"kind"`
	Text              string               `json:"text"`
	CreatedAt         int64                `json:"created_at"`
	AttemptGeneration string               `json:"attempt_generation,omitempty"`
	AcknowledgedAt    int64                `json:"acknowledged_at,omitempty"`
	EscalatedAt       int64                `json:"escalated_at,omitempty"`
	EscalationNote    string               `json:"escalation_note,omitempty"`
	ResolvedAt        int64                `json:"resolved_at,omitempty"`
	ResolutionNote    string               `json:"resolution_note,omitempty"`
	Replies           []CommunicationReply `json:"replies,omitempty"`
}

type CommunicationInbox struct {
	Messages   []CommunicationMessage `json:"messages"`
	NextCursor *int                   `json:"next_cursor,omitempty"`
	Pending    int                    `json:"pending"`
	Escalated  int                    `json:"escalated"`
	Degraded   bool                   `json:"degraded"`
	Workers    []SessionStatus        `json:"workers"`
}

type CommunicationDiagnostics struct {
	Messages   []CommunicationMessage `json:"messages"`
	NextCursor *int                   `json:"next_cursor,omitempty"`
	Pending    int                    `json:"pending"`
	Escalated  int                    `json:"escalated"`
	Degraded   bool                   `json:"degraded"`
}

// The CLI validates the action-specific strict shape before invoking the backend.
type CommunicationRequest struct {
	Protocol   int                  `json:"protocol"`
	Action     string               `json:"action"`
	Caller     *CommunicationCaller `json:"caller,omitempty"`
	JobID      string               `json:"job_id,omitempty"`
	Target     *SessionTarget       `json:"target,omitempty"`
	Name       string               `json:"name,omitempty"`
	Cwd        string               `json:"cwd,omitempty"`
	Task       string               `json:"task,omitempty"`
	MessageID  string               `json:"message_id,omitempty"`
	MessageIDs []string             `json:"message_ids,omitempty"`
	Kind       string               `json:"kind,omitempty"`
	Text       string               `json:"text,omitempty"`
	Note       string               `json:"note,omitempty"`
	RequestID  string               `json:"request_id,omitempty"`
	Cursor     int                  `json:"cursor,omitempty"`
}

type CommunicationCapability struct {
	Protocol int `json:"protocol"`
}

type CommunicationResult struct {
	Capability *CommunicationCapability `json:"capability,omitempty"`
	Manager    *CommunicationManager    `json:"manager,omitempty"`
	Binding    *CommunicationBinding    `json:"binding,omitempty"`
	Released   bool                     `json:"released,omitempty"`
	Inbox      *CommunicationInbox      `json:"inbox,omitempty"`
	Messages   *[]CommunicationMessage  `json:"messages,omitempty"`
	Message    *CommunicationMessage    `json:"message,omitempty"`
	Receipt    *ControlReceipt          `json:"receipt,omitempty"`
	Started    *JobFacts                `json:"started,omitempty"`
}
