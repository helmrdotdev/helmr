package audit

import (
	"encoding/json"
	"fmt"
	"time"
)

// Details contains only the facts permitted for an administrative action.
// Append checks the concrete type against the action before serializing it.
// Request bodies and arbitrary metadata must never be copied into an audit event.
type Details interface{ auditDetails() }

type OwnershipChangedDetails struct {
	WasOwner bool `json:"was_owner"`
	IsOwner  bool `json:"is_owner"`
}

func (OwnershipChangedDetails) auditDetails() {}

type RoleChangedDetails struct {
	OldRole string `json:"old_role"`
	NewRole string `json:"new_role"`
}

type RoleGrantedDetails struct {
	NewRole string `json:"new_role"`
}

type RoleRemovedDetails struct {
	OldRole string `json:"old_role"`
}

type InvitationCreatedDetails struct {
	ExpiresAt *time.Time `json:"expires_at"`
}

type MembershipDetails struct {
	UserID       string `json:"user_id,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	MembershipID string `json:"membership_id"`
	IsOwner      bool   `json:"is_owner"`
}
type InvitationStateDetails struct{}

func (MembershipDetails) auditDetails()      {}
func (InvitationStateDetails) auditDetails() {}

type APIKeyIssuedDetails struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type APIKeyPolicy struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}
type APIKeyChangedDetails struct {
	Before APIKeyPolicy `json:"before"`
	After  APIKeyPolicy `json:"after"`
}

func (APIKeyChangedDetails) auditDetails() {}

type PermissionsDetails struct {
	Permissions []string `json:"permissions"`
}

type RetentionChangedDetails struct {
	OldRetentionSeconds *int64 `json:"old_retention_seconds"`
	NewRetentionSeconds *int64 `json:"new_retention_seconds"`
}

func (RoleChangedDetails) auditDetails()       {}
func (RoleGrantedDetails) auditDetails()       {}
func (RoleRemovedDetails) auditDetails()       {}
func (InvitationCreatedDetails) auditDetails() {}
func (APIKeyIssuedDetails) auditDetails()      {}
func (PermissionsDetails) auditDetails()       {}
func (RetentionChangedDetails) auditDetails()  {}

// AccessChangeDetails contains policy facts, never request metadata or credentials.
type AccessSnapshot struct {
	APIKeyID   string   `json:"api_key_id,omitempty"`
	APIKeyName string   `json:"api_key_name,omitempty"`
	GrantID    string   `json:"grant_id,omitempty"`
	AgentsMode string   `json:"agents_mode,omitempty"`
	AgentIDs   []string `json:"agent_ids,omitempty"`
	AgentKeys  []string `json:"agent_keys,omitempty"`
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
type AccessChangeDetails struct {
	CauseAgentID      string            `json:"cause_agent_id,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	BeforeAssignments []*AccessSnapshot `json:"before_assignments,omitempty"`
	AfterAssignments  []*AccessSnapshot `json:"after_assignments,omitempty"`
	Before            *AccessSnapshot   `json:"before"`
	After             *AccessSnapshot   `json:"after"`
	AffectedGrantIDs  []string          `json:"affected_grant_ids,omitempty"`
}

func (AccessChangeDetails) auditDetails() {}

type AgentCleanupDetails struct {
	CauseAgentID string `json:"cause_agent_id"`
	Reason       string `json:"reason"`
}

func (AgentCleanupDetails) auditDetails() {}

type AgentChangeDetails struct {
	Revision           int64   `json:"revision"`
	EdgesRemoved       int     `json:"selected_agent_edges_removed"`
	AssignmentsRemoved int     `json:"ineffective_assignments_removed"`
	MixedPreserved     int     `json:"mixed_assignments_preserved"`
	ConnectionID       *string `json:"slack_connection_disconnected"`
	ActivationsSealed  int64   `json:"trigger_activations_sealed"`
}

func (AgentChangeDetails) auditDetails() {}

func marshalDetails(action string, details Details) ([]byte, error) {
	valid := false
	switch action {
	case "slack_connection.disconnected":
		if details == nil {
			return []byte("{}"), nil
		}
		d, ok := details.(AgentCleanupDetails)
		valid = ok && d.CauseAgentID != "" && d.Reason == "agent_deleted"
	case "agent.deleted", "agent.restored":
		d, ok := details.(AgentChangeDetails)
		valid = ok && d.Revision > 0
	case "member.ownership_changed":
		_, valid = details.(OwnershipChangedDetails)
	case "access.recovered", "permission_group.created", "permission_group.updated", "permission_group.deleted", "permission_group.member_added", "permission_group.member_removed", "role.created", "role.updated", "role.deleted", "role.standard_created", "role_grant.created", "role_grant.updated", "role_grant.deleted":
		d, ok := details.(AccessChangeDetails)
		valid = ok && (d.Before != nil || d.After != nil)
	case "member.role_changed":
		d, ok := details.(RoleChangedDetails)
		valid = ok && d.OldRole != "" && d.NewRole != ""
	case "member.granted", "member.removed", "invitation.accepted":
		d, ok := details.(MembershipDetails)
		valid = ok && d.MembershipID != ""
	case "installation_admin.granted":
		d, ok := details.(RoleGrantedDetails)
		valid = ok && d.NewRole != ""
	case "invitation.revoked", "invitation.expired":
		_, valid = details.(InvitationStateDetails)
	case "invitation.created":
		d, ok := details.(InvitationCreatedDetails)
		valid = ok && d.ExpiresAt != nil && !d.ExpiresAt.IsZero()
		d.ExpiresAt = utcTime(d.ExpiresAt)
		details = d
	case "api_key.updated":
		d, ok := details.(APIKeyChangedDetails)
		valid = ok && d.Before.Name != "" && d.After.Name != ""
	case "api_key.issued":
		d, ok := details.(APIKeyIssuedDetails)
		valid = ok && d.Name != ""
		d.ExpiresAt = utcTime(d.ExpiresAt)
		details = d
	case "slack_connection.authorized":
		d, ok := details.(PermissionsDetails)
		valid = ok && validPermissions(d.Permissions)
	case "project.retention_changed":
		d, ok := details.(RetentionChangedDetails)
		valid = ok && validRetention(d.OldRetentionSeconds) && validRetention(d.NewRetentionSeconds)
	default:
		// Actions without additional facts persist an object, never JSON null.
		if details == nil {
			return []byte("{}"), nil
		}
	}
	if !valid {
		return nil, fmt.Errorf("invalid audit details for %s", action)
	}
	data, err := json.Marshal(details)
	if err != nil {
		return nil, fmt.Errorf("marshal audit details for %s: %w", action, err)
	}
	return data, nil
}

func validRetention(seconds *int64) bool { return seconds == nil || *seconds > 0 }

func validPermissions(permissions []string) bool {
	if len(permissions) == 0 {
		return false
	}
	for _, permission := range permissions {
		if permission == "" {
			return false
		}
	}
	return true
}

// Keep persisted timestamps independent of the database client's local time zone.
func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
