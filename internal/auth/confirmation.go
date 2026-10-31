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

// RecentProofWindow is how long a sign-in or confirmation counts as recent proof.
const RecentProofWindow = time.Hour

// ProofClockSkew is how far a proof time may lie after now, allowing for the
// database and application clocks to differ slightly.
const ProofClockSkew = time.Minute

// ProvedRecently reports whether a proof made at at is still recent at now.
func ProvedRecently(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) <= RecentProofWindow && at.Sub(now) <= ProofClockSkew
}

// RequireRecentProof is called after admission. Activity does not refresh
// proof time. Slack sends security changes to the browser. A CLI login was
// approved from a browser signed in within the last hour, and that approval is
// its proof, so CLI sessions never need recent proof again.
func RequireRecentProof(p Principal) error {
	if p.Kind != PrincipalKindSession && p.Kind != PrincipalKindOperator {
		return nil
	}
	if p.Kind == PrincipalKindSession && p.Channel == "cli" && p.SlackLinkID == uuid.Nil() {
		return nil
	}
	if p.SlackLinkID != uuid.Nil() || !ProvedRecently(p.AuthenticatedAt, time.Now()) {
		return ErrRecentProofRequired
	}
	return nil
}

// Viewing, ordinary work, unprivileged invitations, creating a Secret under a
// new name and connection setup stay outside confirmation; a connection's OAuth
// authorization is its own proof. Group membership changes require recent
// proof only when the Group carries Role assignments, which its owner checks.
// Machine credentials retain their independently configured permission boundary.
func RequireRecentFor(p Principal, permission Permission) error {
	switch permission {
	case PermissionRolesCreate, PermissionRolesUpdate, PermissionRolesDelete,
		PermissionAssignmentsCreate, PermissionAssignmentsUpdate, PermissionAssignmentsDelete,
		PermissionGroupsDelete,
		PermissionMembersRemove, PermissionAPIKeysCreate, PermissionAPIKeysUpdate, PermissionAPIKeysRotate, PermissionAPIKeysRevoke,
		PermissionSecretsRotate, PermissionSecretsRevoke,
		PermissionAgentConnectionsDelete,
		PermissionAgentsDelete, PermissionAgentsRestore, PermissionComputersDelete,
		PermissionComputersUpdate:
		return RequireRecentProof(p)
	}
	return nil
}
