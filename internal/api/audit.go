package api

import (
	"encoding/json"
	"time"
	"uuid"
)

// AdminAuditEvent is the full captured evidence, available only through Admin.
type AdminAuditEvent struct {
	RuntimeGeneration *int64              `json:"runtime_generation"`
	ProcessEpoch      *int64              `json:"process_epoch"`
	ID                uuid.UUID           `json:"id"`
	OrgID             *uuid.UUID          `json:"org_id"`
	ProjectID         *uuid.UUID          `json:"project_id"`
	EnvironmentID     *uuid.UUID          `json:"environment_id"`
	SchemaVersion     int                 `json:"schema_version"`
	OccurredAt        time.Time           `json:"occurred_at"`
	RecordedAt        time.Time           `json:"recorded_at"`
	RetentionSeconds  int64               `json:"retention_seconds"`
	ExpiresAt         time.Time           `json:"expires_at"`
	ActorUserID       *uuid.UUID          `json:"actor_user_id"`
	ActorAPIKeyID     *uuid.UUID          `json:"actor_api_key_id"`
	ActorSessionID    *uuid.UUID          `json:"actor_session_id"`
	ActorWorkerHostID *uuid.UUID          `json:"actor_worker_host_id"`
	ActorComponent    *string             `json:"actor_component"`
	ActorIdentity     *AuditIdentity      `json:"actor_identity"`
	Authentication    AuditAuthentication `json:"authentication"`
	Action            string              `json:"action"`
	Outcome           string              `json:"outcome"`
	TargetKind        string              `json:"target_kind"`
	TargetID          *uuid.UUID          `json:"target_id"`
	TargetUserID      *uuid.UUID          `json:"target_user_id"`
	SlackTeamID       *string             `json:"slack_team_id"`
	SlackUserID       *string             `json:"slack_user_id"`
	TargetResolution  string              `json:"target_resolution"`
	Authorization     json.RawMessage     `json:"authorization"`
	OperationID       uuid.UUID           `json:"operation_id"`
	SessionID         *uuid.UUID          `json:"session_id"`
	TurnID            *uuid.UUID          `json:"turn_id"`
	ComputerID        *uuid.UUID          `json:"computer_id"`
	DeploymentID      *uuid.UUID          `json:"deployment_id"`
	CauseEventID      *uuid.UUID          `json:"cause_event_id"`
	Causality         json.RawMessage     `json:"causality"`
	SourceIP          *string             `json:"source_ip"`
	Channel           string              `json:"channel"`
	Details           json.RawMessage     `json:"details"`
}
type AuditIdentity struct {
	Source      string `json:"source"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
}
type AuditBefore struct {
	RecordedAt time.Time `json:"recorded_at"`
	ID         uuid.UUID `json:"id"`
}
type AdminAuditEventsResponse struct {
	Events     []AdminAuditEvent `json:"events"`
	NextBefore *AuditBefore      `json:"next_before,omitempty"`
}

type AuditAuthentication struct {
	Result            string     `json:"result"`
	Origin            string     `json:"origin"`
	Provider          *string    `json:"provider"`
	CredentialKind    *string    `json:"credential_kind"`
	CredentialID      *uuid.UUID `json:"credential_id"`
	CredentialVersion *int64     `json:"credential_version"`
}

// AuditAction is one action the audit trail can record, with the category it
// is listed under when people filter for it.
type AuditAction struct {
	Action     string   `json:"action"`
	TargetKind string   `json:"target_kind"`
	Outcomes   []string `json:"outcomes"`
	Category   string   `json:"category"`
}

type AuditActionsResponse struct {
	Actions []AuditAction `json:"actions"`
}
