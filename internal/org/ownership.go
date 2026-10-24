package org

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// RecoverOwnAccess explicitly restores only the acting owner's fixed access Role.
func RecoverOwnAccess(ctx context.Context, database db.TxBeginner, m ManagingMember) (RoleGrant, error) {
	var result RoleGrant
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, m.OrgID); err != nil {
			return err
		}
		var member uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND id=$2 AND user_id=$3 AND is_owner AND removed_at IS NULL FOR UPDATE`, m.OrgID, m.MemberID, m.UserID).Scan(&member); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrOwnerRoleRequired
			}
			return err
		}
		var role uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM roles WHERE org_id=$1 AND builtin_key='access_administrator' FOR UPDATE`, m.OrgID).Scan(&role); err != nil {
			return err
		}
		id := uuid.NewV7()
		tag, err := tx.Exec(ctx, `INSERT INTO role_grants(id,org_id,role_id,member_id,created_by) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, id, m.OrgID, role, member, m.UserID)
		if err != nil {
			return err
		}
		created := tag.RowsAffected() != 0
		if !created {
			if err := tx.QueryRow(ctx, `SELECT id FROM role_grants WHERE org_id=$1 AND role_id=$2 AND member_id=$3 AND environment_id IS NULL`, m.OrgID, role, member).Scan(&id); err != nil {
				return err
			}
		}
		grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{ID: id})
		if err != nil {
			return err
		}
		if len(grants) != 1 {
			return ErrAccessNotFound
		}
		result = grants[0]
		result.Created = created
		if !created {
			return nil
		}
		return accessAudit(ctx, tx, m, "access.recovered", "role_grant", id, nil, grantSnapshot(result), nil)
	})
	return result, err
}

type OwnershipMember struct {
	MembershipID uuid.UUID `json:"membership_id"`
	DisplayName  string    `json:"display_name"`
	IsOwner      bool      `json:"is_owner"`
}

// OwnershipMembers discloses only active identities needed for owner succession.
func OwnershipMembers(ctx context.Context, database db.TxBeginner, m ManagingMember) ([]OwnershipMember, error) {
	result := []OwnershipMember{}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var owner bool
		if err := tx.QueryRow(ctx, `SELECT is_owner FROM org_members WHERE org_id=$1 AND id=$2 AND user_id=$3 AND removed_at IS NULL`, m.OrgID, m.MemberID, m.UserID).Scan(&owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrOwnerRoleRequired
			}
			return err
		}
		if !owner {
			return ErrOwnerRoleRequired
		}
		rows, err := tx.Query(ctx, `SELECT m.id,COALESCE(m.display_name,u.display_name),m.is_owner FROM org_members m JOIN users u ON u.id=m.user_id WHERE m.org_id=$1 AND m.removed_at IS NULL ORDER BY m.is_owner DESC,m.id`, m.OrgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var member OwnershipMember
			if err := rows.Scan(&member.MembershipID, &member.DisplayName, &member.IsOwner); err != nil {
				return err
			}
			result = append(result, member)
		}
		return rows.Err()
	})
	return result, err
}
