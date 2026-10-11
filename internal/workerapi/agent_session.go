package workerapi

import (
	"encoding/json"
	"time"
)

// RuntimeSession identifies a guest-owned process. The authenticated HTTP worker
// supplies Host identity/epoch outside this request; customer JSON cannot set it.
type RuntimeSession struct {
	EnvironmentID      string `json:"environment_id"`
	SessionID          string `json:"session_id"`
	ProcessEpoch       int64  `json:"process_epoch"`
	ComputerLeaseEpoch int64  `json:"computer_lease_epoch"`
}
type AgentAuthorityResponse struct {
	AuthorityGeneration int64     `json:"authority_generation"`
	ExpiresAt           time.Time `json:"expires_at"`
}
type AgentAttachmentResponse struct {
	Stopped             bool      `json:"stopped,omitempty"`
	Starting            bool      `json:"starting,omitempty"`
	AuthorityGeneration int64     `json:"authority_generation"`
	AttachmentSequence  int64     `json:"attachment_sequence"`
	ExpiresAt           time.Time `json:"expires_at"`
}

type AgentControlRequest struct {
	Session            RuntimeSession `json:"session"`
	AttachmentSequence int64          `json:"attachment_sequence"`
}
type AgentControlResponse struct {
	Sequence            int64     `json:"sequence"`
	AuthorityGeneration int64     `json:"authority_generation"`
	Kind                string    `json:"kind"`
	Acknowledged        bool      `json:"acknowledged"`
	Error               string    `json:"error,omitempty"`
	ExpiresAt           time.Time `json:"expires_at"`
}
type AgentControlReceipt struct {
	Session             RuntimeSession `json:"session"`
	AttachmentSequence  int64          `json:"attachment_sequence"`
	Sequence            int64          `json:"sequence"`
	AuthorityGeneration int64          `json:"authority_generation"`
	Kind                string         `json:"kind"`
	Error               string         `json:"error,omitempty"`
}

type AgentOperationRequest struct {
	Session             RuntimeSession  `json:"session"`
	RequestID           string          `json:"request_id"`
	AuthorityGeneration int64           `json:"authority_generation"`
	TurnID              string          `json:"turn_id,omitempty"`
	Method              int32           `json:"method"`
	Payload             json.RawMessage `json:"payload"`
	DrainEvidence       string          `json:"drain_evidence,omitempty"`
}
type AgentOperationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type AgentOperationResponse struct {
	RequestID string               `json:"request_id"`
	Value     json.RawMessage      `json:"value,omitempty"`
	Error     *AgentOperationError `json:"error,omitempty"`
}

// AgentStartResponse is issued only for a current starting process. Program is
// resolved from the Session's pinned, verified deployment bundle.
type AgentStartResponse struct {
	Secrets          []SecretDelivery       `json:"secrets"`
	ProtectedEnv     *ProtectedEnv          `json:"protected_env,omitempty"`
	BundleDigest     string                 `json:"bundle_digest"`
	Authority        AgentAuthorityResponse `json:"authority"`
	Program          RuntimeProgram         `json:"program"`
	AgentKey         string                 `json:"agent_key"`
	ComputerID       string                 `json:"computer_id"`
	SessionKey       *string                `json:"session_key,omitempty"`
	ParentSessionID  *string                `json:"parent_session_id,omitempty"`
	TerminalSequence int64                  `json:"terminal_sequence"`
}

type AgentStartReleaseRequest struct {
	Session            RuntimeSession `json:"session"`
	AttachmentSequence int64          `json:"attachment_sequence"`
	BundleDigest       string         `json:"bundle_digest"`
}
