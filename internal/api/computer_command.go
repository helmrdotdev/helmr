package api

import "time"

type ExecuteComputerRequest struct {
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	StdinBase64    string            `json:"stdin_base64,omitempty"`
	Timeout        string            `json:"timeout,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

type CommandReceipt struct {
	CommandID string `json:"command_id"`
}

type CommandInfo struct {
	ID                string          `json:"id"`
	ComputerID        string          `json:"computer_id"`
	Status            string          `json:"status"`
	ProcessReconciled bool            `json:"process_reconciled"`
	Outcome           *CommandOutcome `json:"outcome,omitempty"`
}

type CommandOutcome struct {
	CommandID  string          `json:"command_id"`
	Kind       string          `json:"kind"`
	TerminalAt time.Time       `json:"terminal_at"`
	ExitCode   *int32          `json:"exit_code,omitempty"`
	Failure    *CommandFailure `json:"failure,omitempty"`
}

type CommandFailure struct {
	Reason string `json:"reason"`
}

// CommandLogRecord preserves bytes and exposes missing sequence ranges explicitly.
type CommandLogRecord struct {
	Kind            string     `json:"kind"`
	Stream          string     `json:"stream"`
	Cursor          string     `json:"cursor"`
	ContentBase64   string     `json:"content_base64,omitempty"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
	FromSequence    string     `json:"from_sequence,omitempty"`
	ThroughSequence string     `json:"through_sequence,omitempty"`
}

type CommandLogPage struct {
	OutputState string             `json:"output_state"`
	Logs        []CommandLogRecord `json:"logs"`
	NextCursor  string             `json:"next_cursor,omitempty"`
}
