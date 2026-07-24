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
