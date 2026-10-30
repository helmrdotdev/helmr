package auth

import (
	"time"
	"uuid"
)

type recentAuthenticationRequired struct{}

func (recentAuthenticationRequired) Error() string {
	return "confirm your sign-in before retrying this action"
}
func (recentAuthenticationRequired) ErrorCode() string { return "recent_authentication_required" }

var ErrRecentProofRequired error = recentAuthenticationRequired{}

// RequireRecentProof is called after current-source admission. Consent/activity
// does not refresh proof time. Slack sends security changes to the browser.
func RequireRecentProof(p Principal) error {
	if p.Kind != PrincipalKindSession && p.Kind != PrincipalKindOperator {
		return nil
	}
	if p.SlackLinkID != uuid.Nil() || p.AuthenticatedAt.IsZero() || time.Since(p.AuthenticatedAt) > time.Hour {
		return ErrRecentProofRequired
	}
	return nil
}

// Viewing, ordinary work and unprivileged invitations stay outside confirmation.
// Machine credentials retain their independently configured permission boundary.
func RequireRecentFor(p Principal, permission Permission) error {
	switch permission {
	case PermissionRolesCreate, PermissionRolesUpdate, PermissionRolesDelete,
		PermissionAssignmentsCreate, PermissionAssignmentsUpdate, PermissionAssignmentsDelete,
		PermissionGroupsDelete, PermissionGroupsAddMember, PermissionGroupsRemoveMember,
		PermissionMembersRemove, PermissionAPIKeysCreate, PermissionAPIKeysUpdate, PermissionAPIKeysRotate, PermissionAPIKeysRevoke,
		PermissionSecretsCreate, PermissionSecretsRotate, PermissionSecretsRevoke,
		PermissionAgentConnectionsConfigure, PermissionAgentConnectionsDelete,
		PermissionAgentsDelete, PermissionAgentsRestore, PermissionComputersDelete,
		PermissionComputersUpdate:
		return RequireRecentProof(p)
	}
	return nil
}
