// Package access reads current workload authority for operation owners. It does
// not write policy; organization operations own Roles, groups and their Grants.
package access

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrDenied = errors.New("permission is required")

// PrincipalUnavailable preserves only the predicate observed during a refresh.
// Not-found predicates do not distinguish revocation, expiry or missing membership.
type PrincipalUnavailable struct {
	UserID, APIKeyID                  uuid.UUID
	Reason                            string
	PresentedVersion, ObservedVersion int64
}

func (e *PrincipalUnavailable) Error() string { return ErrDenied.Error() }
func (e *PrincipalUnavailable) Unwrap() error { return ErrDenied }

// Principal resolves a human or machine against an exact environment using the
// caller's transaction snapshot. A schedule/runtime Session is not a human.
func Principal(ctx context.Context, database db.DBTX, kind string, id, env uuid.UUID) (auth.Principal, error) {
	var p auth.Principal
	var project, org uuid.UUID
	if err := database.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, env).Scan(&org, &project); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, ErrDenied
		}
		return p, err
	}
	p.OrgID, p.ProjectID, p.EnvironmentID = org, project.String(), env.String()
	switch kind {
	case "user":
		human, err := OrganizationPrincipal(ctx, database, org, id)
		if err != nil {
			return p, err
		}
		human.ProjectID, human.EnvironmentID = p.ProjectID, p.EnvironmentID
		return human, nil
	case "api_key":
		p.Kind, p.APIKeyID = auth.PrincipalKindAPIKey, id
		var key uuid.UUID
		if err := database.QueryRow(ctx, `SELECT id,credential_version FROM api_keys WHERE id=$1 AND org_id=$2 AND environment_id=$3 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>statement_timestamp())`, id, org, env).Scan(&key, &p.CredentialVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return p, &PrincipalUnavailable{APIKeyID: id, Reason: "current_key_not_found"}
			}
			return p, err
		}
		grants, err := APIKeyGrants(ctx, database, org, id)
		if err != nil {
			return p, err
		}
		p.Grants = grants
		return p, nil
	default:
		return p, ErrDenied
	}
}

// OrganizationPrincipal loads the active membership and its current additive Grants.
// Ownership is identity metadata; it never supplies ordinary permissions.
func OrganizationPrincipal(ctx context.Context, database db.DBTX, org, user uuid.UUID) (auth.Principal, error) {
	p := auth.Principal{OrgID: org, UserID: user, Kind: auth.PrincipalKindSession}
	if err := database.QueryRow(ctx, `SELECT id,is_owner FROM org_members WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL`, org, user).Scan(&p.MemberID, &p.IsOwner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, &PrincipalUnavailable{UserID: user, Reason: "active_membership_not_found"}
		}
		return p, err
	}
	grants, err := MemberGrants(ctx, database, org, p.MemberID)
	if err != nil {
		return p, err
	}
	for _, g := range grants {
		p.Grants = append(p.Grants, g.PermissionGrant)
	}
	return p, nil
}

func Check(ctx context.Context, database db.DBTX, kind string, id, env, agent uuid.UUID, permission auth.Permission) error {
	p, err := Principal(ctx, database, kind, id, env)
	if err != nil {
		return err
	}
	scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID}
	if agent != uuid.Nil() {
		scope.AgentID = agent.String()
	}
	if permissionErr := auth.Require(p, permission, scope,
		ErrDenied); permissionErr != nil {
		return permissionErr
	}
	return nil
}

// Current binds a transport identity to its still-active exact environment.
func Current(ctx context.Context, q db.DBTX, prior auth.Principal, scope auth.Scope) (auth.Principal, error) {
	if scope.EnvironmentID == "" && scope.ProjectID == "" && scope.AgentID == "" {
		if prior.Kind != auth.PrincipalKindSession || (scope.OrgID != uuid.Nil() && scope.OrgID != prior.OrgID) {
			return auth.Principal{}, ErrDenied
		}
		return CurrentHuman(ctx, q, prior)
	}
	env, err := uuid.Parse(scope.EnvironmentID)
	if err != nil {
		return auth.Principal{}, ErrDenied
	}
	kind, id := "user", prior.UserID
	if prior.Kind == auth.PrincipalKindAPIKey {
		kind, id = "api_key", prior.APIKeyID
	} else if prior.Kind != auth.PrincipalKindSession {
		return auth.Principal{}, ErrDenied
	}
	if prior.Kind == auth.PrincipalKindSession {
		p, err := CurrentHuman(ctx, q, prior)
		if err != nil {
			return p, err
		}
		var org, project uuid.UUID
		if err = q.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, env).Scan(&org, &project); err != nil {
			return p, humanReadError(err)
		}
		if p.OrgID != org || (scope.OrgID != uuid.Nil() && scope.OrgID != org) || scope.ProjectID != project.String() {
			return p, ErrDenied
		}
		p.ProjectID, p.EnvironmentID = project.String(), env.String()
		return p, nil
	}
	p, err := Principal(ctx, q, kind, id, env)
	if err != nil {
		return p, err
	}
	if p.OrgID != prior.OrgID || p.ProjectID != scope.ProjectID || scope.OrgID != uuid.Nil() && scope.OrgID != p.OrgID {
		return auth.Principal{}, ErrDenied
	}
	if prior.Kind == auth.PrincipalKindAPIKey && prior.CredentialVersion != p.CredentialVersion {
		return auth.Principal{}, &PrincipalUnavailable{APIKeyID: prior.APIKeyID, Reason: "credential_version_changed", PresentedVersion: prior.CredentialVersion, ObservedVersion: p.CredentialVersion}
	}
	if prior.Kind == auth.PrincipalKindSession {
		if prior.MemberID != uuid.Nil() && p.MemberID != prior.MemberID {
			return auth.Principal{}, ErrDenied
		}
		p.SessionID = prior.SessionID
	}
	return p, nil
}
