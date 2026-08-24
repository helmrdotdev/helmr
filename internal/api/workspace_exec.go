package api

type ExecuteWorkspaceRequest struct {
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	StdinBase64    string            `json:"stdin_base64,omitempty"`
	Timeout        string            `json:"timeout,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

type ExecuteWorkspaceResult struct {
	ExitCode     int32  `json:"exit_code"`
	StdoutBase64 string `json:"stdout_base64"`
	StderrBase64 string `json:"stderr_base64"`
}

type WorkspaceExecProcessStatus string

const (
	WorkspaceExecProcessStatusPending WorkspaceExecProcessStatus = "pending"
	WorkspaceExecProcessStatusRunning WorkspaceExecProcessStatus = "running"
	WorkspaceExecProcessStatusExited  WorkspaceExecProcessStatus = "exited"
	WorkspaceExecProcessStatusFailed  WorkspaceExecProcessStatus = "failed"
)

// WorkspaceExecProcess is the public admission and polling projection. It is
// intentionally separate from ExecuteWorkspaceResult, which is also used by
// the internal Worker wire.
type WorkspaceExecProcess struct {
	ProcessID    string                     `json:"process_id"`
	Status       WorkspaceExecProcessStatus `json:"status"`
	ExitCode     *int32                     `json:"exit_code,omitempty"`
	StdoutBase64 *string                    `json:"stdout_base64,omitempty"`
	StderrBase64 *string                    `json:"stderr_base64,omitempty"`
	Error        *WorkspaceExecProcessError `json:"error,omitempty"`
}

type WorkspaceExecProcessError struct {
	TerminalReasonCode string `json:"terminal_reason_code"`
}
