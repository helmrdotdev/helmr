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
	NewRole   string     `json:"new_role"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type APIKeyIssuedDetails struct {
	NewRole     string     `json:"new_role"`
	Permissions []string   `json:"permissions"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

type PermissionsDetails struct {
	Permissions []string `json:"permissions"`
}

type RetentionChangedDetails struct {
	OldRetentionMode    string `json:"old_retention_mode"`
	OldRetentionSeconds *int64 `json:"old_retention_seconds"`
	NewRetentionMode    string `json:"new_retention_mode"`
	NewRetentionSeconds *int64 `json:"new_retention_seconds"`
}

func (RoleChangedDetails) auditDetails()       {}
func (RoleGrantedDetails) auditDetails()       {}
func (RoleRemovedDetails) auditDetails()       {}
func (InvitationCreatedDetails) auditDetails() {}
func (APIKeyIssuedDetails) auditDetails()      {}
func (PermissionsDetails) auditDetails()       {}
func (RetentionChangedDetails) auditDetails()  {}

func marshalDetails(action string, details Details) ([]byte, error) {
	valid := false
	switch action {
	case "member.role_changed":
		d, ok := details.(RoleChangedDetails)
		valid = ok && d.OldRole != "" && d.NewRole != ""
	case "member.granted", "installation_admin.granted", "invitation.accepted":
		d, ok := details.(RoleGrantedDetails)
		valid = ok && d.NewRole != ""
	case "member.removed", "invitation.revoked", "invitation.expired":
		d, ok := details.(RoleRemovedDetails)
		valid = ok && d.OldRole != ""
	case "invitation.created":
		d, ok := details.(InvitationCreatedDetails)
		valid = ok && d.NewRole != "" && d.ExpiresAt != nil && !d.ExpiresAt.IsZero()
		d.ExpiresAt = utcTime(d.ExpiresAt)
		details = d
	case "api_key.issued":
		d, ok := details.(APIKeyIssuedDetails)
		valid = ok && d.NewRole != "" && validPermissions(d.Permissions)
		d.ExpiresAt = utcTime(d.ExpiresAt)
		details = d
	case "slack_connection.authorized":
		d, ok := details.(PermissionsDetails)
		valid = ok && validPermissions(d.Permissions)
	case "environment.retention_changed":
		d, ok := details.(RetentionChangedDetails)
		valid = ok && validRetention(d.OldRetentionMode, d.OldRetentionSeconds) && validRetention(d.NewRetentionMode, d.NewRetentionSeconds)
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

func validRetention(mode string, seconds *int64) bool {
	switch mode {
	case "duration":
		return seconds != nil && *seconds > 0
	case "until_environment_deletion":
		return seconds == nil
	default:
		return false
	}
}

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
