package access

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SessionPrincipal authenticates the envelope and a live source. A browser has
// no implicit organization; a CLI bearer is fixed to its exact membership.
func SessionPrincipal(ctx context.Context, database db.DBTX, sessionID, target uuid.UUID) (auth.Principal, error) {
	row, err := db.New(database).GetLoginSession(ctx, pgvalue.UUID(sessionID))
	if err != nil {
		return auth.Principal{}, humanReadError(err)
	}
	if row.Channel != "browser" && row.Channel != "cli" {
		return auth.Principal{}, ErrDenied
	}
	p := auth.Principal{Kind: auth.PrincipalKindSession, SessionID: sessionID, UserID: pgvalue.MustUUIDValue(row.UserID), Channel: row.Channel}
	p.IdentityID, p.PasskeyID, p.AuthenticatedAt = nullableID(row.IdentityID), nullableID(row.PasskeyID), row.AuthenticatedAt.Time
	if row.Channel == "cli" {
		p.OrgID, p.MemberID = pgvalue.MustUUIDValue(row.OrgID), pgvalue.MustUUIDValue(row.MemberID)
		if target != uuid.Nil() && target != p.OrgID {
			return p, ErrDenied
		}
	} else {
		p.OrgID = target
	}
	if p.OrgID != uuid.Nil() {
		return attachMembership(ctx, database, p)
	}
	return p, nil
}

func nullableID(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.Nil()
	}
	return pgvalue.MustUUIDValue(id)
}
func humanReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}

func attachMembership(ctx context.Context, q db.DBTX, p auth.Principal) (auth.Principal, error) {
	var id uuid.UUID
	var cutoff pgtype.Timestamptz
	err := q.QueryRow(ctx, `SELECT id,is_owner,reauthenticate_after FROM org_members
 WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL AND suspended_at IS NULL FOR SHARE`, p.OrgID, p.UserID).Scan(&id, &p.IsOwner, &cutoff)
	if err != nil {
		return p, humanReadError(err)
	}
	if p.MemberID != uuid.Nil() && p.MemberID != id {
		return p, ErrDenied
	}
	if cutoff.Valid && !p.AuthenticatedAt.After(cutoff.Time) {
		return p, ErrDenied
	}
	p.MemberID = id
	grants, err := MemberGrants(ctx, q, p.OrgID, id)
	if err != nil {
		return p, err
	}
	p.Grants = nil
	for _, g := range grants {
		p.Grants = append(p.Grants, g.PermissionGrant)
	}
	return p, nil
}

// currentMember checks current account and organization access for a new human operation.
func currentMember(ctx context.Context, q db.DBTX, prior auth.Principal) (auth.Principal, error) {
	if prior.Kind != auth.PrincipalKindSession || prior.UserID == uuid.Nil() || prior.MemberID == uuid.Nil() || prior.AuthenticatedAt.IsZero() {
		return auth.Principal{}, ErrDenied
	}
	var locked uuid.UUID
	if err := q.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, prior.UserID).Scan(&locked); err != nil {
		return prior, humanReadError(err)
	}
	if err := q.QueryRow(ctx, `SELECT id FROM org_members WHERE id=$1 AND org_id=$2 AND user_id=$3 AND removed_at IS NULL AND suspended_at IS NULL FOR SHARE`, prior.MemberID, prior.OrgID, prior.UserID).Scan(&locked); err != nil {
		return prior, humanReadError(err)
	}
	return attachMembership(ctx, q, prior)
}

// CurrentHuman additionally requires the live transport authority for new
// operations, pending consent and streams. The sign-in method a session or link
// recorded is history, not authority: removing that method does not end it.
func CurrentHuman(ctx context.Context, q db.DBTX, prior auth.Principal) (auth.Principal, error) {
	current, err := currentMember(ctx, q, prior)
	if err != nil {
		return prior, err
	}
	var locked uuid.UUID
	var active bool
	if prior.SlackLinkID != uuid.Nil() {
		if err := q.QueryRow(ctx, `SELECT l.id FROM slack_user_links l JOIN slack_installations i ON i.id=l.installation_id WHERE l.id=$1 FOR SHARE OF l,i`, prior.SlackLinkID).Scan(&locked); err != nil {
			return prior, humanReadError(err)
		}
		err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_user_links l
 JOIN slack_installations i ON i.id=l.installation_id AND i.org_id=l.org_id AND i.team_id=l.team_id
 WHERE l.id=$1 AND l.user_id=$2 AND l.org_id=$3 AND l.member_id=$4 AND l.revoked_at IS NULL
 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL)`, prior.SlackLinkID, prior.UserID, prior.OrgID, prior.MemberID).Scan(&active)
	} else {
		if err := q.QueryRow(ctx, `SELECT id FROM login_sessions WHERE id=$1 FOR SHARE`, prior.SessionID).Scan(&locked); err != nil {
			return prior, humanReadError(err)
		}
		err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM login_sessions s
 WHERE s.id=$1 AND s.user_id=$2 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND (s.channel='browser' OR (s.channel='cli' AND s.org_id=$3 AND s.member_id=$4)))`, prior.SessionID, prior.UserID, prior.OrgID, prior.MemberID).Scan(&active)
	}
	if err != nil {
		return prior, err
	}
	if !active {
		return prior, ErrDenied
	}
	return current, nil
}

// SlackPrincipal loads only an exact independently issued link. A later link
// for the same Slack actor never restores authority saved under an older ID.
// The link's issue time stands in for a sign-in time; member and account
// retirement revoke the link itself.
func SlackPrincipal(ctx context.Context, q db.DBTX, id uuid.UUID) (auth.Principal, error) {
	p := auth.Principal{Kind: auth.PrincipalKindSession, SlackLinkID: id, Channel: "slack"}
	err := q.QueryRow(ctx, `SELECT org_id,member_id,user_id,linked_at FROM slack_user_links WHERE id=$1 AND revoked_at IS NULL`, id).Scan(&p.OrgID, &p.MemberID, &p.UserID, &p.AuthenticatedAt)
	if err != nil {
		return p, humanReadError(err)
	}
	return CurrentHuman(ctx, q, p)
}
