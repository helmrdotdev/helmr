package identity

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"time"
	"uuid"
)

type InvitationSummary struct {
	OrganizationID   uuid.UUID `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	Email            string    `json:"email"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func ResolveInvitationSummary(ctx context.Context, q db.DBTX, cfg Config, token string) (InvitationSummary, error) {
	var result InvitationSummary
	hash, err := auth.HashToken(cfg.invitationKey, token)
	if err != nil {
		return result, ErrInvalidToken
	}
	err = q.QueryRow(ctx, `SELECT i.org_id,o.name,i.invitee_email,i.expires_at FROM invitations i JOIN organizations o ON o.id=i.org_id WHERE i.token_hash=$1 AND i.revoked_at IS NULL AND (i.accepted_at IS NOT NULL OR i.expires_at>clock_timestamp())`, hash).Scan(&result.OrganizationID, &result.OrganizationName, &result.Email, &result.ExpiresAt)
	if isNoRows(err) {
		return result, ErrInvalidToken
	}
	return result, err
}

type AcceptedInvitation struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	MembershipID   uuid.UUID `json:"membership_id"`
}

func AcceptAccountInvitation(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, token string) (AcceptedInvitation, error) {
	var result AcceptedInvitation
	if p.Channel != "browser" {
		return result, ErrAccountProofRequired
	}
	hash, err := auth.HashToken(cfg.invitationKey, token)
	if err != nil {
		return result, ErrInvalidToken
	}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id,org_id FROM invitations WHERE token_hash=$1`, hash).Scan(&id, &result.OrganizationID); err != nil {
			if isNoRows(err) {
				return ErrInvalidToken
			}
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, result.OrganizationID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		current, err := access.SessionPrincipal(ctx, tx, p.SessionID, uuid.Nil())
		if err != nil {
			return err
		}
		if current.UserID != p.UserID {
			return ErrInvalidToken
		}
		var email, name, canonical string
		var member pgtype.UUID
		var expiry time.Time
		var revoked pgtype.Timestamptz
		if err = tx.QueryRow(ctx, `SELECT invitee_email,accepted_by_member_id,expires_at,revoked_at FROM invitations WHERE id=$1 FOR UPDATE`, id).Scan(&email, &member, &expiry, &revoked); err != nil {
			return err
		}
		if revoked.Valid {
			return ErrInvalidToken
		}
		if err = tx.QueryRow(ctx, `SELECT primary_email,display_name FROM users WHERE id=$1`, p.UserID).Scan(&canonical, &name); err != nil {
			return err
		}
		if member.Valid {
			var owner uuid.UUID
			if err = tx.QueryRow(ctx, `SELECT user_id FROM org_members WHERE id=$1 AND org_id=$2 AND removed_at IS NULL AND suspended_at IS NULL`, member, result.OrganizationID).Scan(&owner); err != nil {
				return ErrInvalidToken
			}
			if owner != p.UserID {
				return ErrWrongAccount
			}
			result.MembershipID = pgvalue.MustUUIDValue(member)
			return nil
		}
		if normalizeEmail(canonical) != normalizeEmail(email) {
			return ErrWrongAccount
		}
		if !time.Now().Before(expiry) {
			return ErrInvalidToken
		}
		// Existing live members consume consent without creating a new lifetime or grants.
		err = tx.QueryRow(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL AND suspended_at IS NULL`, result.OrganizationID, p.UserID).Scan(&result.MembershipID)
		if err == nil {
			return org.AcceptExistingMemberInvitation(ctx, tx, cfg.auditPolicy, audit.FromPrincipal(current), id, result.OrganizationID, result.MembershipID, p.UserID)
		}
		if !isNoRows(err) {
			return err
		}
		err = org.AcceptInvitation(ctx, tx, cfg.auditPolicy, audit.FromPrincipal(current), org.PendingInvitation{ID: id, OrgID: result.OrganizationID, InviteeEmail: email}, p.UserID, name)
		if err != nil {
			return acceptanceError(err)
		}
		return tx.QueryRow(ctx, `SELECT accepted_by_member_id FROM invitations WHERE id=$1`, id).Scan(&result.MembershipID)
	})
	return result, err
}
