package dbtest

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	role := uuid.NewV7()
	MustExec(t, t.Context(), q, `INSERT INTO roles(id,org_id,name) VALUES($1,$2,$1::uuid::text)`, role, org)
	MustExec(t, t.Context(), q, `INSERT INTO role_permissions(org_id,role_id,permission) SELECT $1,$2,unnest($3::text[])`, org, role, permissions)
	mode := "none"
	for _, p := range permissions {
		if auth.CapabilityResourceType(auth.Permission(p)) == "agents" {
			mode = "all"
		}
	}
	MustExec(t, t.Context(), q, `INSERT INTO role_grants(id,org_id,role_id,member_id,environment_id,agents_mode) VALUES($1,$2,$3,$4,$5,$6)`, uuid.NewV7(), org, role, member, environment, mode)
	return member
}

// CreateRole creates a fixture Role and returns its matching Agent target mode.
func CreateRole(t *testing.T, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, org uuid.UUID, permissions ...string) (uuid.UUID, string) {
	t.Helper()
	role := uuid.NewV7()
	MustExec(t, t.Context(), q, `INSERT INTO roles(id,org_id,name) VALUES($1,$2,$1::uuid::text)`, role, org)
	MustExec(t, t.Context(), q, `INSERT INTO role_permissions(org_id,role_id,permission) SELECT $1,$2,p FROM unnest($3::text[]) p ON CONFLICT DO NOTHING`, org, role, permissions)
	mode := "none"
	for _, p := range permissions {
		if auth.CapabilityResourceType(auth.Permission(p)) == "agents" {
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
	MustExec(t, t.Context(), q, `INSERT INTO role_grants(id,org_id,role_id,api_key_id,environment_id,agents_mode) VALUES($1,$2,$3,$4,$5,$6)`, uuid.NewV7(), org, role, key, env, mode)
	return role
}
