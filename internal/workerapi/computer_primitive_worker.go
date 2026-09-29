package workerapi

import (
	"context"
	"encoding/json"
	"time"
)

type ComputerCommandClaimRequest struct {
	ActiveCancellationIDs []string `json:"active_cancellation_ids,omitempty"`
	ActiveCommandIDs      []string `json:"active_command_ids,omitempty"`
	OrgID                 string   `json:"org_id"`
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

type ComputerCommandCompleteRequest struct {
	OrgID              string          `json:"org_id"`
	CommandID          string          `json:"command_id"`
	ComputerInstanceID string          `json:"computer_instance_id"`
	WriterGeneration   int64           `json:"writer_generation"`
	Outcome            string          `json:"outcome"`
	ExitCode           *int32          `json:"exit_code,omitempty"`
	Error              json.RawMessage `json:"error,omitempty"`
}

type ComputerRunCleanup struct {
	RunID         string `json:"run_id"`
	RunLeaseID    string `json:"run_lease_id"`
	AttemptNumber uint32 `json:"attempt_number"`
}
type ComputerRunCleanupRequest struct {
	EnvironmentID      string `json:"environment_id"`
	ComputerInstanceID string `json:"computer_instance_id"`
	WriterGeneration   int64  `json:"writer_generation"`
}
type ComputerRunCleanupResponse struct {
	Run *ComputerRunCleanup `json:"run,omitempty"`
}
type ComputerRunReconcileRequest struct {
	ComputerRunCleanupRequest
	ComputerRunCleanup
}

type ComputerServerControlPlaneClient interface {
	GetComputerRunCleanup(context.Context, ComputerRunCleanupRequest) (ComputerRunCleanupResponse, error)
	ReconcileComputerRun(context.Context, ComputerRunReconcileRequest) error
	RenewComputerInstance(context.Context, ComputerInstanceRenewRequest) (ComputerInstanceRenewResponse, error)
	MarkComputerInstanceClosed(context.Context, ComputerInstanceStateRequest) (ComputerInstance, error)
	MarkComputerInstanceFailed(context.Context, ComputerInstanceStateRequest) (ComputerInstance, error)
	ClaimComputerCommand(context.Context, ComputerCommandClaimRequest) (ComputerCommandClaimResponse, error)
	CompleteComputerCommand(context.Context, ComputerCommandCompleteRequest) error
	ReconcileComputerCommand(context.Context, ComputerCommandCompleteRequest) error
	AppendCommandLog(context.Context, CommandLogAppendRequest) error
}

type ComputerCommandCancellation struct {
	CommandID          string    `json:"command_id"`
	ComputerID         string    `json:"computer_id"`
	ComputerInstanceID string    `json:"computer_instance_id"`
	WriterGeneration   int64     `json:"writer_generation"`
	RequestFingerprint string    `json:"request_fingerprint"`
	ExpiresAt          time.Time `json:"expires_at"`
}
