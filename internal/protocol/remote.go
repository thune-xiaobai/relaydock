package protocol

type ShellInfo struct {
	Kind           string `json:"kind"`
	Executable     string `json:"executable"`
	MaxRunning     int    `json:"max_running"`
	MaxTimeoutMS   int    `json:"max_timeout_ms"`
	MaxOutputBytes int64  `json:"max_output_bytes"`
}
type RemoteArgs struct {
	JobID     string `json:"job_id"`
	Command   string `json:"command,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
type RemoteJob struct {
	ID          string `json:"id"`
	Node        string `json:"node"`
	CallID      string `json:"call_id"`
	Command     string `json:"command"`
	CWD         string `json:"cwd"`
	Shell       string `json:"shell"`
	Status      string `json:"status"`
	Revision    int    `json:"revision"`
	CreatedAt   string `json:"created_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	TimeoutMS   int    `json:"timeout_ms"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Error       string `json:"error,omitempty"`
	StdoutBytes int64  `json:"stdout_bytes"`
	StderrBytes int64  `json:"stderr_bytes"`
	Truncated   bool   `json:"truncated"`
}

func (j RemoteJob) Terminal() bool {
	switch j.Status {
	case "succeeded", "failed", "cancelled", "timed_out", "unknown":
		return true
	}
	return false
}

type RemoteSnapshot struct {
	Job             RemoteJob `json:"job"`
	Stdout          string    `json:"stdout"`
	Stderr          string    `json:"stderr"`
	Cursor          string    `json:"cursor"`
	NextCursor      string    `json:"next_cursor"`
	HasMore         bool      `json:"has_more"`
	CancelRequested bool      `json:"cancel_requested,omitempty"`
}
