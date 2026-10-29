package org

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// grantEnvironmentCreatorAccess runs only while creating an Environment, after
// the creation operation has authorized and locked the active creator. Creation
// includes these explicit grants; it does not require assignments.create.
// Removing a grant later never causes it to be recreated.
func grantEnvironmentCreatorAccess(ctx context.Context, tx pgx.Tx, m ManagingMember, environmentID uuid.UUID) error {
	for _, key := range []string{"environment_resource_administrator", "auditor"} {
		var roleID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM roles WHERE org_id=$1 AND builtin_key=$2`, m.OrgID, key).Scan(&roleID); err != nil {
			return err
		}
		id := uuid.NewV7()
		if _, err := tx.Exec(ctx, `INSERT INTO role_grants(id,org_id,role_id,member_id,environment_id,all_agents,created_by_user_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, m.OrgID, roleID, m.MemberID, environmentID, key == "environment_resource_administrator", m.UserID); err != nil {
			return err
		}
		grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{ID: id})
		if err != nil {
			return err
		}
		if len(grants) != 1 {
			return ErrAccessNotFound
		}
		if err := accessAudit(ctx, tx, m, "role_grant.created", "role_grant", id, nil, grantSnapshot(grants[0])); err != nil {
			return err
		}
	}
	return nil
}
