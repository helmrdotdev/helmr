package api

import (
	"encoding/json"
	"time"
	"uuid"
)

// AuditEvent is security metadata. It never contains execution input or output.
type AuditEvent struct {
	ID                   uuid.UUID       `json:"id"`
	OrgID                *uuid.UUID      `json:"org_id"`
	ProjectID            *uuid.UUID      `json:"project_id"`
	EnvironmentID        *uuid.UUID      `json:"environment_id"`
	SchemaVersion        int             `json:"schema_version"`
	OccurredAt           time.Time       `json:"occurred_at"`
	RecordedAt           time.Time       `json:"recorded_at"`
	RetentionSeconds     int64           `json:"retention_seconds"`
	ExpiresAt            time.Time       `json:"expires_at"`
	ActorKind            string          `json:"actor_kind"`
	ActorID              *uuid.UUID      `json:"actor_id"`
	ActorService         *string         `json:"actor_service"`
	ActorMemberID        *uuid.UUID      `json:"actor_member_id"`
	ActorIdentity        *AuditIdentity  `json:"actor_identity"`
	CredentialKind       string          `json:"credential_kind"`
	CredentialID         *uuid.UUID      `json:"credential_id"`
	CredentialVersion    *int64          `json:"credential_version"`
	RuntimeGeneration    *int64          `json:"runtime_generation"`
	ProcessEpoch         *int64          `json:"process_epoch"`
	Action               string          `json:"action"`
	Outcome              string          `json:"outcome"`
	TargetKind           string          `json:"target_kind"`
	TargetID             *uuid.UUID      `json:"target_id"`
	TargetUserID         *uuid.UUID      `json:"target_user_id"`
	SlackTeamID          *string         `json:"slack_team_id"`
	SlackUserID          *string         `json:"slack_user_id"`
	TargetResolution     string          `json:"target_resolution"`
	AuthorizationWitness json.RawMessage `json:"authorization_witness"`
	OperationID          uuid.UUID       `json:"operation_id"`
	SessionID            *uuid.UUID      `json:"session_id"`
	TurnID               *uuid.UUID      `json:"turn_id"`
	ComputerID           *uuid.UUID      `json:"computer_id"`
	DeploymentID         *uuid.UUID      `json:"deployment_id"`
	CauseEventID         *uuid.UUID      `json:"cause_event_id"`
	Causality            json.RawMessage `json:"causality"`
	SourceIP             *string         `json:"source_ip"`
	Channel              string          `json:"channel"`
	Details              json.RawMessage `json:"details"`
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
type AuditEventsResponse struct {
	Events     []AuditEvent `json:"events"`
	NextBefore *AuditBefore `json:"next_before,omitempty"`
}
