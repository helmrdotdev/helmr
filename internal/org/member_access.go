package org

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// ChangeMemberAccess invalidates only this membership's prior proof. Suspension
// preserves grants and credentials; resumption still requires a fresh proof.
// A nil suspended value is immediate revocation without a permanent ban.
func ChangeMemberAccess(ctx context.Context, tx pgx.Tx, manager ManagingMember, id uuid.UUID, suspended *bool) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, manager.OrgID); err != nil {
		return err
	}
	if err := lockActiveMembers(ctx, tx, manager, id); err != nil {
		return err
	}
	if err := manager.Authorize(ctx, tx, auth.PermissionMembersRemove); err != nil {
		return err
	}
	target, err := activeMember(ctx, db.New(tx), manager.OrgID, id)
	if err != nil {
		return err
	}
	if suspended != nil && *suspended && target.IsOwner {
		return ErrOwnerRoleRequired
	}
	action := "member.sessions_revoked"
	if suspended != nil {
		if *suspended {
			action = "member.suspended"
		} else {
			action = "member.resumed"
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE org_members SET updated_at=clock_timestamp(),reauthenticate_after=clock_timestamp(),suspended_at=CASE WHEN $3::boolean IS NULL THEN suspended_at WHEN $3 THEN clock_timestamp() ELSE NULL END WHERE org_id=$1 AND id=$2 AND removed_at IS NULL`, manager.OrgID, id, suspended); err != nil {
		return err
	}
	return audit.Append(ctx, tx, manager.AuditPolicy, audit.Event{Evidence: manager.Actor, OrgID: manager.OrgID, Action: action, TargetKind: "member", TargetID: id, Details: audit.MembershipDetails{MembershipID: id.String(), UserID: pgvalue.UUIDString(target.UserID), DisplayName: target.DisplayName.String, IsOwner: target.IsOwner}})
}
