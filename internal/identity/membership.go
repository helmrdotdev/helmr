package identity

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/slack"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// CreateOrganization commits the organization and owner membership together
// with a verified independent operator during initial Helmr setup.
func CreateOrganization(ctx context.Context, database db.TxBeginner, input org.OrganizationInput, account, operator auth.Principal) (db.Organization, error) {
	if input.Actor.Authorization.Decision == "" {
		input.Actor.Authorization = audit.NotEvaluated()
	}
	var organization db.Organization
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := requireRecentAccount(ctx, tx, account); err != nil {
			return err
		}
		if input.OwnerUserID != account.UserID {
			return ErrInvalidToken
		}
		if input.InitialSetup {
			current, err := operatorFromSession(ctx, tx, operator.SessionID)
			if err != nil {
				return err
			}
			if current.OperatorID != operator.OperatorID || !access.Recent(current, time.Now()) {
				return ErrRecentProofRequired
			}
		}
		var err error
		organization, err = org.CreateOrganization(ctx, tx, input)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return db.Organization{}, err
	}
	return organization, nil
}

// RemoveMember retires only the exact membership's independent channels. Browser
// account proof and other organizations remain usable; the removed member cannot.
func RemoveMember(ctx context.Context, database db.TxBeginner, manager org.ManagingMember, memberID uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := org.DisableMember(ctx, tx, manager, memberID); err != nil {
			return err
		}
		if err := slack.RetireMemberAccess(ctx, tx, manager.OrgID, memberID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE org_id=$1 AND member_id=$2 AND revoked_at IS NULL RETURNING id`, manager.OrgID, memberID)
		if err != nil {
			return err
		}
		var sessions []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			sessions = append(sessions, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range sessions {
			if err := appendSessionLifecycle(ctx, tx, Config{auditPolicy: manager.AuditPolicy}, manager.Actor, pgvalue.UUID(manager.OrgID), "identity.session_revoked", id, audit.SessionRevokedDetails{Reason: "membership_removed"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// AcceptInvitation runs in the caller's transaction, changing membership before
// revoking existing login sessions. Login can then issue a new session in the
// same transaction without an intermediate authority change.
func AcceptInvitation(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, invitation org.PendingInvitation, user uuid.UUID, displayName string) error {
	if err := org.AcceptInvitation(ctx, tx, policy, actor, invitation, user, displayName); err != nil {
		return err
	}
	return nil
}

// ChangeMemberAccess retires independent member channels after the org owner
// records the cutoff; ordinary account proof and other memberships survive.
func ChangeMemberAccess(ctx context.Context, database db.TxBeginner, manager org.ManagingMember, id uuid.UUID, suspended *bool) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := org.ChangeMemberAccess(ctx, tx, manager, id, suspended); err != nil {
			return err
		}
		if err := slack.RetireMemberAccess(ctx, tx, manager.OrgID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE org_id=$1 AND member_id=$2 AND revoked_at IS NULL`, manager.OrgID, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE device_codes SET status='denied',consumed_at=NULL WHERE org_id=$1 AND decided_by_member_id=$2 AND status='approved'`, manager.OrgID, id)
		return err
	})
}
