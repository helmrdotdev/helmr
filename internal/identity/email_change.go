package identity

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"uuid"
)

func completeEmailChange(ctx context.Context, tx pgx.Tx, cfg Config, challenge uuid.UUID, user, session pgtype.UUID, epoch pgtype.Int8, email, raw string) (string, error) {
	if !user.Valid || !session.Valid || !epoch.Valid {
		return "", ErrInvalidToken
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", ErrInvalidToken
	}
	var matches bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM login_sessions WHERE id=$1 AND user_id=$2 AND token_hash=$3 AND user_access_epoch=$4 AND revoked_at IS NULL AND expires_at>clock_timestamp() AND channel='browser')`, session, user, hash, epoch.Int64).Scan(&matches); err != nil {
		return "", err
	}
	if !matches {
		return "", ErrInvalidToken
	}
	p, err := access.SessionPrincipal(ctx, tx, pgvalue.MustUUIDValue(session), uuid.Nil())
	if err != nil {
		return "", err
	}
	if p.AccessEpoch != epoch.Int64 {
		return "", ErrInvalidToken
	}
	if err = requireRecentAccount(ctx, tx, p); err != nil {
		return "", err
	}
	var old string
	if err = tx.QueryRow(ctx, `SELECT primary_email FROM users WHERE id=$1`, user).Scan(&old); err != nil {
		return "", err
	}
	if old == email {
		return "", invalidInput("new email must differ from current email")
	}
	var occupied bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(primary_email)=$1 AND id<>$2)`, email, user).Scan(&occupied); err != nil {
		return "", err
	}
	if occupied {
		return "", ErrIdentityConflict
	}
	rows, readErr := tx.Query(ctx, `SELECT id FROM auth_identities WHERE user_id=$1 AND provider='email' AND revoked_at IS NULL`, user)
	if readErr != nil {
		return "", readErr
	}
	oldSources, readErr := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if readErr != nil {
		return "", readErr
	}
	for _, source := range oldSources {
		if err := slack.RetireAccountAccess(ctx, tx, pgvalue.MustUUIDValue(user), source); err != nil {
			return "", err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_identities SET revoked_at=clock_timestamp(),updated_at=clock_timestamp() WHERE user_id=$1 AND provider='email' AND revoked_at IS NULL`, user); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE browser_account_proofs p SET revoked_at=clock_timestamp() FROM auth_identities i WHERE p.identity_id=i.id AND i.user_id=$1 AND i.provider='email' AND p.revoked_at IS NULL`, user); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET primary_email=$2,updated_at=clock_timestamp() WHERE id=$1`, user, email); err != nil {
		return "", err
	}
	credential, err := db.New(tx).CreateAuthIdentity(ctx, db.CreateAuthIdentityParams{ID: pgvalue.UUID(uuid.NewV7()), UserID: user, Provider: "email", Subject: pgvalue.MustUUIDValue(user).String(), ObservedEmail: pgtype.Text{String: email, Valid: true}})
	if err != nil {
		return "", err
	}
	proof := provedUser(pgvalue.MustUUIDValue(user), pgvalue.MustUUIDValue(credential.ID), uuid.Nil(), epoch.Int64, credential.LastAuthenticatedAt.Time, "email")
	token, err := confirmBrowserProof(ctx, tx, cfg, pgvalue.MustUUIDValue(session), hash, proof)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE email_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, challenge); err != nil {
		return "", err
	}
	// Other mailbox-change requests were bound to the old address and must restart.
	if _, err = tx.Exec(ctx, `UPDATE email_challenges SET revoked_at=clock_timestamp() WHERE user_id=$1 AND id<>$2 AND consumed_at IS NULL AND revoked_at IS NULL`, user, challenge); err != nil {
		return "", err
	}
	if err = accountSecurityEvent(ctx, tx, cfg, p, "identity.email_changed", nullableUUID(credential.ID), "email"); err != nil {
		return "", err
	}
	if err = enqueueSecurityNotification(ctx, tx, p.UserID, "identity.email_changed", old); err != nil {
		return "", err
	}
	return token, nil
}

func CheckEmailChangeChallenge(ctx context.Context, q db.DBTX, p auth.Principal, id uuid.UUID, binding []byte) error {
	var matches bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM email_challenges WHERE id=$1 AND browser_hash=$2 AND purpose='email_change' AND user_id=$3 AND session_id=$4 AND access_epoch=$5 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp())`, id, binding, p.UserID, p.SessionID, p.AccessEpoch).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return ErrInvalidToken
	}
	return nil
}
