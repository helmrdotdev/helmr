package dbtest

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"slices"
	"testing"
	"uuid"
)

// GrantMember explicitly assigns fixture permissions to the current membership.
// A nil environment denotes organization scope; no future environments inherit it.
func GrantMember(t *testing.T, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, org, user uuid.UUID, environment *uuid.UUID, permissions ...string) uuid.UUID {
	t.Helper()
	var member uuid.UUID
	if err := q.QueryRow(t.Context(), `SELECT id FROM org_members WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL`, org, user).Scan(&member); err != nil {
		t.Fatal(err)
	}
	permissions = slices.Clone(permissions)
	slices.Sort(permissions)
	permissions = slices.Compact(permissions)
	role := uuid.NewV7()
	MustExec(t, t.Context(), q, `INSERT INTO roles(id,org_id,name,permissions) VALUES($1,$2,$1::uuid::text,$3)`, role, org, permissions)
	mode := "none"
	for _, p := range permissions {
		if auth.IsAgentPermission(auth.Permission(p)) {
			mode = "all"
		}
	}
	MustExec(t, t.Context(), q, `INSERT INTO role_grants(id,org_id,role_id,member_id,environment_id,all_agents) VALUES($1,$2,$3,$4,$5,$6)`, uuid.NewV7(), org, role, member, environment, mode == "all")
	return member
}

// CreateRole creates a fixture Role and returns its matching Agent target mode.
func CreateRole(t *testing.T, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, org uuid.UUID, permissions ...string) (uuid.UUID, string) {
	t.Helper()
	permissions = slices.Clone(permissions)
	slices.Sort(permissions)
	permissions = slices.Compact(permissions)
	role := uuid.NewV7()
	MustExec(t, t.Context(), q, `INSERT INTO roles(id,org_id,name,permissions) VALUES($1,$2,$1::uuid::text,$3)`, role, org, permissions)
	mode := "none"
	for _, p := range permissions {
		if auth.IsAgentPermission(auth.Permission(p)) {
			mode = "all"
		}
	}
	return role, mode
}

// SetAPIKeyRole replaces a fixture key's assignments with one explicit Role.
// Production issuance/assignment tests should use their owning operations instead.
func SetAPIKeyRole(t *testing.T, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, key uuid.UUID, permissions ...string) uuid.UUID {
	t.Helper()
	var org, env uuid.UUID
	if err := q.QueryRow(t.Context(), `SELECT org_id,environment_id FROM api_keys WHERE id=$1`, key).Scan(&org, &env); err != nil {
		t.Fatal(err)
	}
	role, mode := CreateRole(t, q, org, permissions...)
	MustExec(t, t.Context(), q, `DELETE FROM role_grants WHERE api_key_id=$1`, key)
	MustExec(t, t.Context(), q, `INSERT INTO role_grants(id,org_id,role_id,api_key_id,environment_id,all_agents) VALUES($1,$2,$3,$4,$5,$6)`, uuid.NewV7(), org, role, key, env, mode == "all")
	return role
}

// SecretManager creates a real, narrowly scoped fixture key. Secret tests use
// the same current-principal authorization path as production mutations.
func SecretManager(t *testing.T, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, environment uuid.UUID) auth.Principal {
	t.Helper()
	key := uuid.NewV7()
	var org, project uuid.UUID
	if err := q.QueryRow(t.Context(), `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash)
 SELECT $1,org_id,project_id,id,'secret-test-manager-'||$1::uuid::text,'fixture',convert_to($1::uuid::text,'UTF8') FROM environments WHERE id=$2 RETURNING org_id,project_id`, key, environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	SetAPIKeyRole(t, q, key, "secrets.create", "secrets.rotate", "secrets.revoke")
	return auth.Principal{Kind: auth.PrincipalKindAPIKey, APIKeyID: key, OrgID: org, ProjectID: project.String(), EnvironmentID: environment.String(), CredentialVersion: 1}
}
