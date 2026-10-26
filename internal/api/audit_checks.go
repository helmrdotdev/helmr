package api

import "uuid"

type AuditCheckScope struct {
	Kind          string    `json:"kind"`
	OrgID         uuid.UUID `json:"org_id,omitzero"`
	ProjectID     uuid.UUID `json:"project_id,omitzero"`
	EnvironmentID uuid.UUID `json:"environment_id,omitzero"`
	SessionID     uuid.UUID `json:"session_id,omitzero"`
}
type AuditMembershipAttributeCheck struct {
	Type      string          `json:"type"`
	MemberID  uuid.UUID       `json:"member_id"`
	Attribute string          `json:"attribute"`
	Expected  bool            `json:"expected"`
	Observed  bool            `json:"observed"`
	Scope     AuditCheckScope `json:"scope"`
	Result    string          `json:"result"`
}

func (AuditMembershipAttributeCheck) auditCheck() {}

type AuditPrincipalCurrencyCheck struct {
	Type              string          `json:"type"`
	SubjectUserID     uuid.UUID       `json:"subject_user_id,omitzero"`
	SubjectAPIKeyID   uuid.UUID       `json:"subject_api_key_id,omitzero"`
	Scope             AuditCheckScope `json:"scope"`
	ObservationSource string          `json:"observation_source"`
	Result            string          `json:"result"`
	Reason            string          `json:"reason"`
	PresentedVersion  int64           `json:"presented_version,omitempty"`
	ObservedVersion   int64           `json:"observed_version,omitempty"`
}

func (AuditPrincipalCurrencyCheck) auditCheck() {}

type AuditOriginCheck struct {
	Type          string    `json:"type"`
	SessionID     uuid.UUID `json:"session_id"`
	InitiatorKind string    `json:"initiator_kind"`
	InitiatorID   uuid.UUID `json:"initiator_id"`
	Result        string    `json:"result"`
}

func (AuditOriginCheck) auditCheck() {}

type AuditSlackThreadCheck struct {
	Type      string    `json:"type"`
	SessionID uuid.UUID `json:"session_id"`
	ThreadID  uuid.UUID `json:"thread_id"`
	UserID    uuid.UUID `json:"user_id"`
	Result    string    `json:"result"`
}

func (AuditSlackThreadCheck) auditCheck() {}

type AuditPermissionScope struct {
	OrgID         uuid.UUID `json:"org_id"`
	AgentID       string    `json:"agent_id,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
	EnvironmentID string    `json:"environment_id,omitempty"`
}
type AuditPermissionWitness struct {
	Permission         string               `json:"permission"`
	Scope              AuditPermissionScope `json:"scope"`
	AssignmentID       string               `json:"assignment_id"`
	AssignmentRevision int64                `json:"assignment_revision"`
	RoleID             string               `json:"role_id"`
	RoleRevision       int64                `json:"role_revision"`
	MemberID           uuid.UUID            `json:"member_id,omitzero"`
	GroupID            string               `json:"group_id,omitempty"`
	Subject            string               `json:"subject"`
}
type AuditPermissionCheck struct {
	Type            string                  `json:"type"`
	SubjectUserID   uuid.UUID               `json:"subject_user_id,omitzero"`
	SubjectAPIKeyID uuid.UUID               `json:"subject_api_key_id,omitzero"`
	Permission      string                  `json:"permission"`
	Scope           *AuditPermissionScope   `json:"scope,omitempty"`
	Result          string                  `json:"result"`
	Reason          string                  `json:"reason,omitempty"`
	Evidence        *AuditPermissionWitness `json:"evidence,omitempty"`
}

func (AuditPermissionCheck) auditCheck() {}
