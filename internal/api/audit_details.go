package api

import (
	"time"
	"uuid"
)

type AuditOwnershipChangedDetails struct {
	WasOwner bool `json:"was_owner"`
	IsOwner  bool `json:"is_owner"`
}

func (AuditOwnershipChangedDetails) auditDetails() {}

type AuditMembershipDetails struct {
	UserID       string `json:"user_id,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	MembershipID string `json:"membership_id"`
	IsOwner      bool   `json:"is_owner"`
}

func (AuditMembershipDetails) auditDetails() {}

type AuditInvitationCreatedDetails struct {
	ExpiresAt *time.Time `json:"expires_at"`
}

func (AuditInvitationCreatedDetails) auditDetails() {}

type AuditInvitationStateDetails struct{}

func (AuditInvitationStateDetails) auditDetails() {}

type AuditAPIKeyIssuedDetails struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (AuditAPIKeyIssuedDetails) auditDetails() {}

type AuditAPIKeyPolicy struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}
type AuditAPIKeyChangedDetails struct {
	Before AuditAPIKeyPolicy `json:"before"`
	After  AuditAPIKeyPolicy `json:"after"`
}

func (AuditAPIKeyChangedDetails) auditDetails() {}

type AuditPermissionsDetails struct {
	Permissions []string `json:"permissions"`
}

func (AuditPermissionsDetails) auditDetails() {}

type AuditRetentionChangedDetails struct {
	OldRetentionSeconds *int64 `json:"old_retention_seconds"`
	NewRetentionSeconds *int64 `json:"new_retention_seconds"`
}

func (AuditRetentionChangedDetails) auditDetails() {}

type AuditAccessSnapshot struct {
	APIKeyID   string   `json:"api_key_id,omitempty"`
	APIKeyName string   `json:"api_key_name,omitempty"`
	GrantID    string   `json:"grant_id,omitempty"`
	AgentsMode string   `json:"agents_mode,omitempty"`
	AgentIDs   []string `json:"agent_ids,omitempty"`
	Revision   int64    `json:"revision,omitempty"`

	MemberName      string   `json:"member_name,omitempty"`
	GroupName       string   `json:"group_name,omitempty"`
	RoleName        string   `json:"role_name,omitempty"`
	ProjectName     string   `json:"project_name,omitempty"`
	EnvironmentName string   `json:"environment_name,omitempty"`
	AgentKey        string   `json:"agent_key,omitempty"`
	Name            string   `json:"name,omitempty"`
	Description     string   `json:"description,omitempty"`
	Permissions     []string `json:"permissions,omitempty"`
	SubjectKind     string   `json:"subject_kind,omitempty"`
	MemberID        string   `json:"member_id,omitempty"`
	GroupID         string   `json:"group_id,omitempty"`
	RoleID          string   `json:"role_id,omitempty"`
	ProjectID       string   `json:"project_id,omitempty"`
	EnvironmentID   string   `json:"environment_id,omitempty"`
	TargetKind      string   `json:"target_kind,omitempty"`
	AgentID         string   `json:"agent_id,omitempty"`
	UserID          string   `json:"user_id,omitempty"`
}
type AuditAccessChangeDetails struct {
	CauseAgentID string               `json:"cause_agent_id,omitempty"`
	Reason       string               `json:"reason,omitempty"`
	Before       *AuditAccessSnapshot `json:"before"`
	After        *AuditAccessSnapshot `json:"after"`
}

func (AuditAccessChangeDetails) auditDetails() {}

type AuditAgentCleanupDetails struct {
	CauseAgentID string `json:"cause_agent_id"`
	Reason       string `json:"reason"`
}

func (AuditAgentCleanupDetails) auditDetails() {}

type AuditAgentChangeDetails struct {
	Revision           int64   `json:"revision"`
	EdgesRemoved       int     `json:"selected_agent_edges_removed"`
	AssignmentsRemoved int     `json:"ineffective_assignments_removed"`
	MixedPreserved     int     `json:"mixed_assignments_preserved"`
	ConnectionID       *string `json:"slack_connection_disconnected"`
	ActivationsSealed  int64   `json:"trigger_activations_sealed"`
}

func (AuditAgentChangeDetails) auditDetails() {}

type AuditMCPDetails struct {
	Tool string `json:"tool"`
}

func (AuditMCPDetails) auditDetails() {}

type AuditSecretExposureDetails struct {
	ExposureID           uuid.UUID `json:"exposure_id"`
	VersionID            uuid.UUID `json:"version_id"`
	RevocationGeneration int64     `json:"revocation_generation"`
	CommandID            uuid.UUID `json:"command_id,omitzero"`
	ImageBuildID         uuid.UUID `json:"image_build_id,omitzero"`
}

func (AuditSecretExposureDetails) auditDetails() {}

type AuditComputerRetentionDetails struct {
	Before *int64 `json:"before_ms"`
	After  *int64 `json:"after_ms"`
}

func (AuditComputerRetentionDetails) auditDetails() {}

type AuditComputerControlDetails struct {
	Reason    string    `json:"reason,omitempty"`
	Kind      string    `json:"kind"`
	RequestID uuid.UUID `json:"request_id"`
}

func (AuditComputerControlDetails) auditDetails() {}

type AuditSessionControlDetails struct {
	RequestID uuid.UUID `json:"request_id"`
	HoldID    uuid.UUID `json:"hold_id,omitzero"`
}

func (AuditSessionControlDetails) auditDetails() {}

type AuditTemplateRefreshDetails struct {
	ImageBuildID uuid.UUID `json:"image_build_id"`
}

func (AuditTemplateRefreshDetails) auditDetails() {}

type AuditAuthenticationDetails struct {
	DeviceCodeID            uuid.UUID `json:"device_code_id,omitzero"`
	Method                  string    `json:"method"`
	Provider                string    `json:"provider,omitempty"`
	Subject                 string    `json:"subject,omitempty"`
	AuthorizingCredentialID uuid.UUID `json:"authorizing_credential_id,omitzero"`
}

func (AuditAuthenticationDetails) auditDetails() {}

type AuditEmptyDetails struct{}

func (AuditEmptyDetails) auditDetails() {}

type AuditFailureDetails struct {
	Operation string `json:"operation,omitempty"`
	Reason    string `json:"reason"`
}

func (AuditFailureDetails) auditDetails() {}

type AuditLifecycleDetails struct {
	Sequence            int64     `json:"sequence,omitempty"`
	RequestID           uuid.UUID `json:"request_id,omitzero"`
	PublicationRevision int64     `json:"publication_revision,omitempty"`
}

func (AuditLifecycleDetails) auditDetails() {}

type AuditAuthorityChangedDetails struct {
	HoldID uuid.UUID `json:"hold_id"`
}

func (AuditAuthorityChangedDetails) auditDetails() {}

type AuditCheckpointDetails struct {
	CheckpointID uuid.UUID `json:"checkpoint_id"`
}

func (AuditCheckpointDetails) auditDetails() {}

type AuditSearchDetails struct {
	ActorKind              string      `json:"actor_kind,omitempty"`
	TargetKind             string      `json:"target_kind,omitempty"`
	TargetUserID           uuid.UUID   `json:"target_user_id,omitzero"`
	SlackTeamID            string      `json:"slack_team_id,omitempty"`
	SlackUserID            string      `json:"slack_user_id,omitempty"`
	RequestedEnvironmentID uuid.UUID   `json:"requested_environment_id,omitzero"`
	From                   time.Time   `json:"from"`
	To                     time.Time   `json:"to"`
	ResultCount            *int        `json:"result_count,omitempty"`
	EnvironmentIDs         []uuid.UUID `json:"environment_ids,omitzero"`
	Actions                []string    `json:"actions,omitempty"`
	OperationID            uuid.UUID   `json:"operation_id,omitzero"`
	EventID                uuid.UUID   `json:"event_id,omitzero"`
	ActorID                uuid.UUID   `json:"actor_id,omitzero"`
	TargetID               uuid.UUID   `json:"target_id,omitzero"`
	Outcome                string      `json:"outcome,omitempty"`
}

func (AuditSearchDetails) auditDetails() {}
