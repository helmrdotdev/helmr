package access

import (
	"context"
	"errors"
	"time"
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
	p := auth.Principal{Kind: auth.PrincipalKindSession, SessionID: sessionID, UserID: pgvalue.MustUUIDValue(row.UserID), AccessEpoch: row.UserAccessEpoch.Int64, Channel: row.Channel}
	var cutoff pgtype.Timestamptz
	var epoch int64
	if err = database.QueryRow(ctx, `SELECT access_epoch,reauthenticate_after FROM users WHERE id=$1`, p.UserID).Scan(&epoch, &cutoff); err != nil {
		return p, humanReadError(err)
	}
	if epoch != p.AccessEpoch {
		return p, ErrDenied
	}
	if row.Channel == "cli" {
		p.OrgID, p.MemberID = pgvalue.MustUUIDValue(row.OrgID), pgvalue.MustUUIDValue(row.MemberID)
		if target != uuid.Nil() && target != p.OrgID {
			return p, ErrDenied
		}
		p.IdentityID = nullableID(row.CliIdentityID)
		p.PasskeyID = nullableID(row.CliPasskeyID)
		p.AuthenticatedAt = row.CliAuthenticatedAt.Time
	} else {
		var identity, key pgtype.UUID
		err = database.QueryRow(ctx, `SELECT p.identity_id,p.passkey_id,p.authenticated_at
   FROM browser_account_proofs p
   LEFT JOIN auth_identities i ON i.id=p.identity_id AND i.user_id=p.user_id
   LEFT JOIN passkey_credentials k ON k.id=p.passkey_id AND k.user_id=p.user_id
   WHERE p.session_id=$1 AND p.user_id=$2 AND p.revoked_at IS NULL
   AND ((i.id IS NOT NULL AND i.revoked_at IS NULL) OR (k.id IS NOT NULL AND k.revoked_at IS NULL))
   ORDER BY p.authenticated_at DESC,p.id DESC LIMIT 1`, sessionID, p.UserID).Scan(&identity, &key, &p.AuthenticatedAt)
		if err != nil {
			return p, humanReadError(err)
		}
		p.IdentityID, p.PasskeyID = nullableID(identity), nullableID(key)
		p.OrgID = target
	}
	if cutoff.Valid && !p.AuthenticatedAt.After(cutoff.Time) {
		return p, ErrDenied
	}
	if err = validateSource(ctx, database, p); err != nil {
		return p, err
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

func validateSource(ctx context.Context, q db.DBTX, p auth.Principal) error {
	var active bool
	err := q.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM auth_identities WHERE id=$1 AND user_id=$3 AND revoked_at IS NULL)
 OR EXISTS(SELECT 1 FROM passkey_credentials WHERE id=$2 AND user_id=$3 AND revoked_at IS NULL)`, p.IdentityID, p.PasskeyID, p.UserID).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return ErrDenied
	}
	return nil
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

// CurrentHuman rechecks the exact admitted source rather than substituting a
// different proof or a later membership for saved human work.
func CurrentHuman(ctx context.Context, q db.DBTX, prior auth.Principal) (auth.Principal, error) {
	if prior.Kind != auth.PrincipalKindSession || prior.UserID == uuid.Nil() || prior.MemberID == uuid.Nil() {
		return auth.Principal{}, ErrDenied
	}
	// Owner locks serialize admission against logout/source/account retirement.
	// Membership has its own lock below; a new membership never replaces it.
	var locked uuid.UUID
	if err := q.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND access_epoch=$2 FOR SHARE`, prior.UserID, prior.AccessEpoch).Scan(&locked); err != nil {
		return prior, humanReadError(err)
	}
	// Membership precedes source/session locks so exact-member retirement can
	// lock this row and retire its channels without cycling with admission.
	if err := q.QueryRow(ctx, `SELECT id FROM org_members WHERE id=$1 AND org_id=$2 AND user_id=$3 AND removed_at IS NULL AND suspended_at IS NULL FOR SHARE`, prior.MemberID, prior.OrgID, prior.UserID).Scan(&locked); err != nil {
		return prior, humanReadError(err)
	}
	if prior.SlackLinkID != uuid.Nil() {
		if err := q.QueryRow(ctx, `SELECT l.id FROM slack_user_links l JOIN slack_installations i ON i.id=l.installation_id WHERE l.id=$1 FOR SHARE OF l,i`, prior.SlackLinkID).Scan(&locked); err != nil {
			return prior, humanReadError(err)
		}
	} else {
		if err := q.QueryRow(ctx, `SELECT id FROM login_sessions WHERE id=$1 FOR SHARE`, prior.SessionID).Scan(&locked); err != nil {
			return prior, humanReadError(err)
		}
	}
	var active bool
	if prior.SlackLinkID != uuid.Nil() {
		err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_user_links l JOIN users u ON u.id=l.user_id
 JOIN slack_installations i ON i.id=l.installation_id AND i.org_id=l.org_id AND i.team_id=l.team_id
   WHERE l.id=$1 AND l.user_id=$2 AND l.org_id=$3 AND l.member_id=$4 AND l.revoked_at IS NULL
   AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL
   AND l.user_access_epoch=u.access_epoch AND u.access_epoch=$5 AND l.authenticated_at=$6
   AND (u.reauthenticate_after IS NULL OR l.authenticated_at>u.reauthenticate_after)
   AND l.identity_id IS NOT DISTINCT FROM $7
   AND l.passkey_id IS NOT DISTINCT FROM $8)`, prior.SlackLinkID, prior.UserID, prior.OrgID, prior.MemberID, prior.AccessEpoch, prior.AuthenticatedAt, optionalID(prior.IdentityID), optionalID(prior.PasskeyID)).Scan(&active)
		if err != nil {
			return prior, err
		}
	} else {
		err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM login_sessions s JOIN users u ON u.id=s.user_id
   WHERE s.id=$1 AND s.user_id=$2 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
   AND s.user_access_epoch=u.access_epoch AND u.access_epoch=$3
   AND (u.reauthenticate_after IS NULL OR $4>u.reauthenticate_after)
   AND ((s.channel='browser' AND EXISTS(SELECT 1 FROM browser_account_proofs p WHERE p.session_id=s.id AND p.revoked_at IS NULL
     AND p.authenticated_at>=$4 AND p.identity_id IS NOT DISTINCT FROM $5 AND p.passkey_id IS NOT DISTINCT FROM $6))
    OR (s.channel='cli' AND s.org_id=$7 AND s.member_id=$8 AND s.cli_authenticated_at=$4
     AND s.cli_identity_id IS NOT DISTINCT FROM $5 AND s.cli_passkey_id IS NOT DISTINCT FROM $6)))`, prior.SessionID, prior.UserID, prior.AccessEpoch, prior.AuthenticatedAt, optionalID(prior.IdentityID), optionalID(prior.PasskeyID), prior.OrgID, prior.MemberID).Scan(&active)
		if err != nil {
			return prior, err
		}
	}
	if !active {
		return prior, ErrDenied
	}
	if err := validateSource(ctx, q, prior); err != nil {
		return prior, err
	}
	return attachMembership(ctx, q, prior)
}

func Recent(p auth.Principal, now time.Time) bool {
	return !p.AuthenticatedAt.IsZero() && !p.AuthenticatedAt.After(now) && now.Sub(p.AuthenticatedAt) <= time.Hour
}

func optionalID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil() {
		return pgtype.UUID{}
	}
	return pgvalue.UUID(id)
}

// SlackPrincipal loads only an exact independently issued link. A later link
// for the same Slack actor never restores authority saved under an older ID.
func SlackPrincipal(ctx context.Context, q db.DBTX, id uuid.UUID) (auth.Principal, error) {
	p := auth.Principal{Kind: auth.PrincipalKindSession, SlackLinkID: id, Channel: "slack"}
	var identity, key pgtype.UUID
	err := q.QueryRow(ctx, `SELECT org_id,member_id,user_id,user_access_epoch,identity_id,passkey_id,authenticated_at FROM slack_user_links WHERE id=$1 AND revoked_at IS NULL`, id).Scan(&p.OrgID, &p.MemberID, &p.UserID, &p.AccessEpoch, &identity, &key, &p.AuthenticatedAt)
	if err != nil {
		return p, humanReadError(err)
	}
	p.IdentityID, p.PasskeyID = nullableID(identity), nullableID(key)
	return CurrentHuman(ctx, q, p)
}
