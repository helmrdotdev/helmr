package identity

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CreateOrganization commits the organization and owner membership together
// with the initial administrator grant when setting up an installation.
func CreateOrganization(ctx context.Context, database db.TxBeginner, input org.OrganizationInput) (db.Organization, error) {
	if input.Actor.Authorization.Basis == "" {
		input.Actor.Authorization = audit.Authorization{Basis: "credential", Decision: "allowed"}
	}
	var organization db.Organization
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var err error
		organization, err = org.CreateOrganization(ctx, tx, input)
		if err != nil {
			return err
		}
		if input.InitialSetup {
			changed, err := db.New(tx).GrantUserAdmin(ctx, pgvalue.UUID(input.OwnerUserID))
			if err != nil {
				return fmt.Errorf("grant initial administrator: %w", err)
			}
			if changed > 0 {
				if err := audit.Append(ctx, tx, input.AuditPolicy, audit.Event{Actor: input.Actor, Action: "installation_admin.granted", TargetKind: "user", TargetID: input.OwnerUserID, Details: audit.RoleGrantedDetails{NewRole: "admin"}}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return db.Organization{}, err
	}
	return organization, nil
}

// RemoveMember disables membership and revokes organization-pinned and unpinned
// login sessions in one transaction, taking membership locks before session locks.
func RemoveMember(ctx context.Context, database db.TxBeginner, manager org.ManagingMember, memberID uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		q := db.New(tx)
		member, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{OrgID: pgvalue.UUID(manager.OrgID), ID: pgvalue.UUID(memberID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return org.ErrMemberNotFound
		}
		if err != nil {
			return err
		}
		if err := manager.Authorize(ctx, tx, auth.PermissionMembersRemove); err != nil {
			return err
		}
		if err := org.DisableMember(ctx, tx, manager, memberID); err != nil {
			return err
		}
		return revokeSessions(ctx, tx, manager.AuditPolicy, manager.Actor, pgvalue.MustUUIDValue(member.UserID), pgvalue.UUID(manager.OrgID), "membership_removed")
	})
}

// AcceptInvitation runs in the caller's transaction, changing membership before
// revoking existing login sessions. Login can then issue a new session in the
// same transaction without an intermediate authority change.
func AcceptInvitation(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Actor, invitation org.PendingInvitation, user uuid.UUID, displayName string) error {
	if err := org.AcceptInvitation(ctx, tx, policy, actor, invitation, user, displayName); err != nil {
		return err
	}
	return revokeSessions(ctx, tx, policy, actor, user, pgtype.UUID{}, "invitation_accepted")
}

func revokeSessions(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Actor, user uuid.UUID, organization pgtype.UUID, method string) error {
	rows, err := tx.Query(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1 AND ($2::uuid IS NULL OR org_id=$2 OR org_id IS NULL) AND revoked_at IS NULL RETURNING id,org_id`, user, organization)
	if err != nil {
		return err
	}
	type revoked struct {
		id  uuid.UUID
		org pgtype.UUID
	}
	var sessions []revoked
	for rows.Next() {
		var s revoked
		if err := rows.Scan(&s.id, &s.org); err != nil {
			rows.Close()
			return err
		}
		sessions = append(sessions, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range sessions {
		if err := appendSessionLifecycle(ctx, tx, Config{auditPolicy: policy}, actor, s.org, "identity.session_revoked", s.id, audit.AuthenticationDetails{Method: method}); err != nil {
			return err
		}
	}
	return nil
}
