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
