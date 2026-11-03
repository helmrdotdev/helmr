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
const RecentProofWindow = 2 * time.Hour

// ProofClockSkew is how far a proof time may lie after now, allowing for the
// database and application clocks to differ slightly.
const ProofClockSkew = time.Minute

// ProvedRecently reports whether a proof made at at is still recent at now.
func ProvedRecently(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) <= RecentProofWindow && at.Sub(now) <= ProofClockSkew
}

// RequireRecentProof is called after admission. Activity does not refresh
// proof time. Slack sends security changes to the browser. A CLI login was
// approved from a browser with recent proof, and that approval is
// its proof, so CLI sessions never need recent proof again. A browser's live
// SSO proof for the organization counts like its ordinary proof.
func RequireRecentProof(p Principal) error {
	if p.Kind != PrincipalKindSession && p.Kind != PrincipalKindOperator {
		return nil
	}
	if p.Kind == PrincipalKindSession && p.Channel == "cli" && p.SlackLinkID == uuid.Nil() {
		return nil
	}
	if p.SlackLinkID != uuid.Nil() {
		return ErrRecentProofRequired
	}
	now := time.Now()
	if p.SSORequired {
		// The organization's SSO proof must be live. Fresh SSO proves it, and so
		// does the account's own passkey or email code; GitHub does not, because
		// it is not an account credential the organization controls.
		if p.SSOProofID != uuid.Nil() && (ProvedRecently(p.SSOAuthenticatedAt, now) || (ProvedRecently(p.AuthenticatedAt, now) && p.ProofProvider != "github")) {
			return nil
		}
		return ErrRecentProofRequired
	}
	if ProvedRecently(p.AuthenticatedAt, now) || (p.SSOProofID != uuid.Nil() && ProvedRecently(p.SSOAuthenticatedAt, now)) {
		return nil
	}
	return ErrRecentProofRequired
}

// Recent proof guards what adds authority or issues credentials. Removing
// authority, revoking credentials, deleting resources, viewing, ordinary work,
// unprivileged invitations, creating a Secret under a new name and connection
// setup stay outside confirmation; a deletion keeps its own preview. Adding a
// member to a Group that carries Role assignments, which its owner checks, and
// resuming a suspended member also require it. Machine credentials retain
// their independently configured permission boundary.
func RequireRecentFor(p Principal, permission Permission) error {
	switch permission {
	case PermissionRolesCreate, PermissionRolesUpdate,
		PermissionAssignmentsCreate, PermissionAssignmentsUpdate,
		PermissionAPIKeysCreate, PermissionAPIKeysUpdate, PermissionAPIKeysRotate:
		return RequireRecentProof(p)
	}
	return nil
}
