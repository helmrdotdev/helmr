package workerapi

import (
	"context"
	"encoding/json"
	"time"
)

type ComputerCommandClaimRequest struct {
	ActiveCancellationIDs []string `json:"active_cancellation_ids,omitempty"`
	ActiveCommandIDs      []string `json:"active_command_ids,omitempty"`
	EnvironmentID         string   `json:"environment_id"`
	ComputerInstanceID    string   `json:"computer_instance_id"`
	WriterGeneration      int64    `json:"writer_generation"`
}

type ComputerCommandClaimResponse struct {
	Cancellation *ComputerCommandCancellation `json:"cancellation,omitempty"`
	Release      *ComputerCommandRelease      `json:"release,omitempty"`
	Command      *ComputerCommand             `json:"command,omitempty"`
}

type ComputerCommandRelease struct {
	ComputerID         string                         `json:"computer_id"`
	RequestFingerprint string                         `json:"request_fingerprint"`
	Completion         ComputerCommandCompleteRequest `json:"completion"`
}

type ComputerCommand struct {
	TailOnly           bool             `json:"tail_only"`
	ProtectedEnv       *ProtectedEnv    `json:"protected_env,omitempty"`
	CommandID          string           `json:"command_id"`
	ComputerID         string           `json:"computer_id"`
	ComputerInstanceID string           `json:"computer_instance_id"`
	RequestFingerprint string           `json:"request_fingerprint"`
	Request            json.RawMessage  `json:"request"`
	Stdin              []byte           `json:"stdin,omitempty"`
	Secrets            []SecretDelivery `json:"secrets"`
	WriterGeneration   int64            `json:"writer_generation"`
	ExpiresAt          time.Time        `json:"expires_at"`
}

type CommandOutputBoundary struct {
	ThroughSequence int64 `json:"through_sequence"`
	Complete        bool  `json:"complete"`
	Gapped          bool  `json:"gapped"`
}
type ComputerCommandCompleteRequest struct {
	Stdout             CommandOutputBoundary `json:"stdout"`
	Stderr             CommandOutputBoundary `json:"stderr"`
	OutputFenced       bool                  `json:"output_fenced"`
	EnvironmentID      string                `json:"environment_id"`
	CommandID          string                `json:"command_id"`
	ComputerInstanceID string                `json:"computer_instance_id"`
	WriterGeneration   int64                 `json:"writer_generation"`
	Outcome            string                `json:"outcome"`
	ExitCode           *int32                `json:"exit_code,omitempty"`
	Error              json.RawMessage       `json:"error,omitempty"`
}

type ComputerCommandCancellation struct {
	CommandID          string    `json:"command_id"`
	ComputerID         string    `json:"computer_id"`
	ComputerInstanceID string    `json:"computer_instance_id"`
	WriterGeneration   int64     `json:"writer_generation"`
	RequestFingerprint string    `json:"request_fingerprint"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// ComputerCommandClient is independent of Session execution and lease renewal.
type ComputerCommandClient interface {
	ClaimComputerCommand(context.Context, ComputerCommandClaimRequest) (ComputerCommandClaimResponse, error)
	CompleteComputerCommand(context.Context, ComputerCommandCompleteRequest) error
	ReconcileComputerCommand(context.Context, ComputerCommandCompleteRequest) error
	AppendCommandLog(context.Context, CommandLogAppendRequest) (DiagnosticLogReceipt, error)
}
