package api

import (
	"time"
	"uuid"
)

// AuditEventMetadata is the public organization evidence envelope. It contains
// no installation actor identifiers, machine fences or raw transport addresses.
type AuditEventMetadata struct {
	ID               uuid.UUID                `json:"id"`
	OrgID            *uuid.UUID               `json:"org_id"`
	ProjectID        *uuid.UUID               `json:"project_id"`
	EnvironmentID    *uuid.UUID               `json:"environment_id"`
	SchemaVersion    int                      `json:"schema_version"`
	OccurredAt       time.Time                `json:"occurred_at"`
	RecordedAt       time.Time                `json:"recorded_at"`
	RetentionSeconds int64                    `json:"retention_seconds"`
	ExpiresAt        time.Time                `json:"expires_at"`
	ActorUserID      *uuid.UUID               `json:"actor_user_id"`
	ActorAPIKeyID    *uuid.UUID               `json:"actor_api_key_id"`
	ActorSessionID   *uuid.UUID               `json:"actor_session_id"`
	ActorIdentity    *AuditIdentity           `json:"actor_identity"`
	Authentication   AuditAuthentication      `json:"authentication"`
	Action           string                   `json:"action"`
	Outcome          string                   `json:"outcome"`
	TargetKind       string                   `json:"target_kind"`
	TargetID         *uuid.UUID               `json:"target_id"`
	TargetUserID     *uuid.UUID               `json:"target_user_id"`
	SlackTeamID      *string                  `json:"slack_team_id"`
	SlackUserID      *string                  `json:"slack_user_id"`
	TargetResolution string                   `json:"target_resolution"`
	OperationID      uuid.UUID                `json:"operation_id"`
	SessionID        *uuid.UUID               `json:"session_id"`
	TurnID           *uuid.UUID               `json:"turn_id"`
	ComputerID       *uuid.UUID               `json:"computer_id"`
	DeploymentID     *uuid.UUID               `json:"deployment_id"`
	Causality        AuditCausality           `json:"causality"`
	Channel          string                   `json:"channel"`
	ActorKind        AuditActorKind           `json:"actor_kind"`
	Restricted       []AuditRestrictedSection `json:"restricted"`
}

type AuditActorKind string

const (
	AuditActorUser            AuditActorKind = "user"
	AuditActorAPIKey          AuditActorKind = "api_key"
	AuditActorSession         AuditActorKind = "session"
	AuditActorPlatformService AuditActorKind = "platform_service"
	AuditActorUnidentified    AuditActorKind = "unidentified"
)

type AuditRestrictedSection string

const (
	AuditRestrictedActor          AuditRestrictedSection = "actor_identity"
	AuditRestrictedAuthentication AuditRestrictedSection = "authentication"
	AuditRestrictedAuthorization  AuditRestrictedSection = "authorization"
	AuditRestrictedRuntime        AuditRestrictedSection = "runtime"
	AuditRestrictedCausality      AuditRestrictedSection = "causality"
	AuditRestrictedCorrelation    AuditRestrictedSection = "correlation"
	AuditRestrictedDetails        AuditRestrictedSection = "details"
	AuditRestrictedTransport      AuditRestrictedSection = "transport"
)

// AuditEvent is constructed by the server's closed projection, never by copying
// a stored JSON object. The action/outcome discriminates its details type.
type AuditEvent struct {
	AuditEventMetadata
	Authorization AuditAuthorization `json:"authorization"`
	Details       AuditDetails       `json:"details"`
}
type AuditEventsResponse struct {
	Events     []AuditEvent `json:"events"`
	NextBefore *AuditBefore `json:"next_before,omitempty"`
}

type AuditCausality struct {
	InitiatorKind string    `json:"initiator_kind,omitempty"`
	InitiatorID   uuid.UUID `json:"initiator_id,omitzero"`
	SourceID      uuid.UUID `json:"source_id,omitzero"`
}

type AuditAuthorization struct {
	Decision              string       `json:"decision"`
	Checks                []AuditCheck `json:"checks"`
	InstallationAuthority bool         `json:"installation_authority"`
}

// AuditCheck and AuditDetails are closed server-output unions. Receive-only
// clients may preserve the already-public JSON for display without decoding them.
type AuditCheck interface{ auditCheck() }
type AuditDetails interface{ auditDetails() }
