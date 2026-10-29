package audit

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"time"
	"uuid"
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

type PlatformAdminGrantedDetails struct {
	Basis string `json:"basis"`
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

func (PlatformAdminGrantedDetails) auditDetails() {}
func (InvitationCreatedDetails) auditDetails()    {}
func (APIKeyIssuedDetails) auditDetails()         {}
func (PermissionsDetails) auditDetails()          {}
func (RetentionChangedDetails) auditDetails()     {}

// AccessChangeDetails contains policy facts, never request metadata or credentials.
type AccessSnapshot struct {
	APIKeyID   string   `json:"api_key_id,omitempty"`
	APIKeyName string   `json:"api_key_name,omitempty"`
	GrantID    string   `json:"grant_id,omitempty"`
	AgentsMode string   `json:"agents_mode,omitempty"`
	AgentIDs   []string `json:"agent_ids,omitempty"`
	Revision   int64    `json:"revision,omitempty"`

	MemberName      string   `json:"member_name,omitempty"`
	GroupName       string   `json:"group_name,omitempty"`
	BuiltinKey      *string  `json:"builtin_key,omitempty"`
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
	CauseAgentID string          `json:"cause_agent_id,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Before       *AccessSnapshot `json:"before"`
	After        *AccessSnapshot `json:"after"`
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

// IdentitySecurityDetails records source references without credential material or mailbox values.
type IdentitySecurityDetails struct {
	CredentialID uuid.UUID `json:"credential_id,omitzero"`
	Method       string    `json:"method,omitempty"`
}

func (IdentitySecurityDetails) auditDetails() {}

func marshalDetails(action string, details Details) ([]byte, error) {
	if _, ok := Lookup(action); !ok {
		return nil, fmt.Errorf("unknown audit action %s", action)
	}
	valid := false
	switch action {
	case "identity.profile_updated", "identity.email_changed", "identity.credential_added", "identity.credential_removed", "identity.access_revoked", "identity.account_repaired", "operator.sign_in", "operator.session_confirmed", "operator.session_revoked", "operator.credential_added", "operator.credential_removed", "operator.repaired":
		d, ok := details.(IdentitySecurityDetails)
		valid = ok && len(d.Method) <= 64
	case "identity.session_revoked":
		d, ok := details.(SessionRevokedDetails)
		valid = ok && (d.Reason == "logout" || d.Reason == "membership_removed" || d.Reason == "invitation_accepted")
	case "message.rejected":
		d, ok := details.(MessageRejectedDetails)
		valid = ok && (d.Reason == "message_rejected" || d.Reason == "callback_failed" || d.Reason == "turn_closed" || d.Reason == "turn_terminated")
	case "identity.sign_in", "identity.device_authorized", "identity.device_started", "identity.device_denied", "identity.magic_link_requested", "identity.email_code_requested":
		d, ok := details.(AuthenticationDetails)
		valid = ok && d.Method != "" && len(d.Method) <= 64 && len(d.Provider) <= 128 && len(d.Subject) <= 512
	case "audit.searched":
		d, ok := details.(SearchDetails)
		valid = ok && !d.From.IsZero() && d.To.After(d.From) && d.ResultCount >= 0 && d.ResultCount <= 500 && (d.ReadModel == "" || d.ReadModel == "organization" || d.ReadModel == "platform")
	case "audit.retention_observed":
		d, ok := details.(RetentionObservedDetails)
		valid = ok && (auditpolicy.Policy{RetentionSeconds: d.RetentionSeconds}).Validate() == nil
	case "audit.purge_completed":
		d, ok := details.(PurgeDetails)
		valid = ok && d.Count > 0 && !d.Cutoff.IsZero() && !d.MinExpiry.IsZero() && !d.MaxExpiry.Before(d.MinExpiry) && !d.MaxExpiry.After(d.Cutoff)
	case "audit.restore_recorded":
		d, ok := details.(RestoreDetails)
		valid = ok && len(d.BackupReference) > 0 && len(d.BackupReference) <= 256 && !d.UnknownFrom.IsZero() && !d.UnknownTo.Before(d.UnknownFrom)
	case "secret.exposed":
		d, ok := details.(SecretExposureDetails)
		valid = ok && d.ExposureID != (SecretExposureDetails{}).ExposureID && d.VersionID != (SecretExposureDetails{}).VersionID && d.RevocationGeneration >= 0
	case "mcp.admitted":
		d, ok := details.(MCPDetails)
		valid = ok && KnownMCPTool(d.Tool)
	case "region.created", "region.updated":
		d, ok := details.(RegionDetails)
		valid = ok && len(d.RegionID) > 0 && len(d.RegionID) <= 128
	case "computer.retention_changed":
		d, ok := details.(ComputerRetentionDetails)
		valid = ok && (d.Before == nil || *d.Before > 0) && (d.After == nil || *d.After > 0)
	case "computer.wake_requested", "computer.hibernate_requested", "computer.wake_settled", "computer.hibernate_settled":
		d, ok := details.(ComputerControlDetails)
		valid = ok && d.RequestID != (ComputerControlDetails{}).RequestID
	case "session.close_requested", "session.cancel_requested", "session.interrupt_requested", "session.resume_requested", "turn.interrupt_requested":
		d, ok := details.(SessionControlDetails)
		valid = ok && d.RequestID != (SessionControlDetails{}).RequestID
	case "template.refresh_requested":
		d, ok := details.(TemplateRefreshDetails)
		valid = ok && d.ImageBuildID != (TemplateRefreshDetails{}).ImageBuildID
	case "session.hold_created", "session.hold_released":
		d, ok := details.(SessionHoldDetails)
		valid = ok && d.HoldID != (SessionHoldDetails{}).HoldID && d.Generation >= 0 && (d.Reason == "storage_pressure" || d.Reason == "process_failed" || d.Reason == "deadline_elapsed" || d.Reason == "stop_deadline_elapsed" || d.Reason == "secret_revoked" || d.Reason == "execution_lost")
	case "computer.restore_completed", "computer.checkpoint_published":
		d, ok := details.(CheckpointDetails)
		valid = ok && d.CheckpointID != (CheckpointDetails{}).CheckpointID && d.Generation >= 0
	case "trigger.content_read", "session.delivery_check_requested", "session.delivery_abandoned", "input.requested", "message.delivered", "turn.interrupted", "computer.created", "command.cancel_requested", "message.admitted", "worker_group.updated", "session.created", "session.closed", "session.cancelled", "session.content_read", "turn.admitted", "turn.completed", "turn.cancelled", "input.answered", "input.cancelled", "command.admitted", "command.completed", "command.cancelled", "command.output_read":
		d, ok := details.(LifecycleDetails)
		valid = ok && d.Sequence >= 0 && d.Generation >= 0

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
	case "access.recovered", "permission_group.created", "permission_group.updated", "permission_group.deleted", "permission_group.member_added", "permission_group.member_removed", "role.created", "role.updated", "role.deleted", "role_grant.created", "role_grant.updated", "role_grant.deleted":
		d, ok := details.(AccessChangeDetails)
		valid = ok && (d.Before != nil || d.After != nil)
	case "member.granted", "member.removed", "member.sessions_revoked", "member.suspended", "member.resumed", "invitation.accepted":
		d, ok := details.(MembershipDetails)
		valid = ok && d.MembershipID != ""
	case "platform_admin.granted":
		d, ok := details.(PlatformAdminGrantedDetails)
		valid = ok && (d.Basis == "initial_setup" || d.Basis == "configured_admin_email")
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
	case "organization.created", "api_key.rotated", "api_key.revoked", "secret.created", "secret.rotated", "secret.revoked", "project.created", "project.updated", "environment.created", "environment.updated", "deployment.published", "deployment.promoted", "slack_app.credentials_configured", "slack_installation.authorization_lost", "slack_user.linked", "slack_user.unlinked", "worker_group.created", "worker_group.token_rotated", "worker_group.activated", "worker_group.paused", "worker_group.drain_started", "worker_group.disabled", "worker_pool.created", "worker_pool.primary_selected", "worker_pool.resource_policy_updated", "worker_pool.drain_started", "worker_pool.disabled", "worker_host.drain_started", "worker_host.marked_lost", "worker_host.provider_absence_confirmed", "worker_scale_in.completed", "runtime.operation_refused", "turn.failed", "command.failed", "mcp.refused", "computer.deleted":
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
	return !slices.Contains(permissions, "")
}

// Keep persisted timestamps independent of the database client's local time zone.
func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
