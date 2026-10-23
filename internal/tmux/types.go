package tmux

// Client always addresses a job's private socket; no default server is used.
type Client struct{ Executable string }

type CreateOptions struct {
	Socket     string
	Config     string
	Session    string
	Executable string
	JobID      string
	RuntimeDir string
	Env        []string
	Width      int
	Height     int
}

type Snapshot struct {
	Owner      string
	Job        string
	Protocol   string
	Session    string
	PaneID     string
	ServerPID  int
	PanePID    int
	PaneDead   bool
	ExitStatus *int
	Clients    int
	Width      int
	Height     int
}
