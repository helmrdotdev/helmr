package identity

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/slack"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type CredentialSummary struct {
	ID         uuid.UUID  `json:"id"`
	Method     string     `json:"method"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func AccountCredentials(ctx context.Context, q db.DBTX, p auth.Principal) ([]CredentialSummary, error) {
	if p.Channel != "browser" || p.UserID == uuid.Nil() {
		return nil, ErrInvalidToken
	}
	rows, err := q.Query(ctx, `SELECT id,provider,provider,created_at,last_authenticated_at FROM auth_identities WHERE user_id=$1 AND provider<>'email' AND revoked_at IS NULL
 UNION ALL SELECT id,'passkey',label,created_at,last_used_at FROM passkey_credentials WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at,id`, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []CredentialSummary{}
	for rows.Next() {
		var c CredentialSummary
		if err = rows.Scan(&c.ID, &c.Method, &c.Label, &c.CreatedAt, &c.LastUsedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func RevokeAccountAccess(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		if err := requireRecentAccount(ctx, tx, p); err != nil {
			return err
		}
		if err := invalidateAccount(ctx, tx, p.UserID); err != nil {
			return err
		}
		return accountSecurityEvent(ctx, tx, cfg, p, "identity.access_revoked", uuid.Nil(), "")
	})
}
func invalidateAccount(ctx context.Context, tx pgx.Tx, user uuid.UUID) error {
	if err := slack.RetireAccountAccess(ctx, tx, user, uuid.Nil()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET access_epoch=access_epoch+1,updated_at=clock_timestamp() WHERE id=$1`, user); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, user); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE email_challenges c SET revoked_at=clock_timestamp() FROM login_sessions s WHERE s.id=c.session_id AND s.user_id=$1 AND c.consumed_at IS NULL`, user); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE webauthn_challenges c SET revoked_at=clock_timestamp() WHERE c.consumed_at IS NULL AND
 (EXISTS(SELECT 1 FROM login_sessions s WHERE s.id=c.session_id AND s.user_id=$1)
 OR EXISTS(SELECT 1 FROM account_repair_enrollments r WHERE r.id=c.account_repair_enrollment_id AND r.user_id=$1))`, user); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE device_codes SET status='denied',consumed_at=NULL WHERE user_id=$1 AND status='approved'`, user); err != nil {
		return err
	}
	return nil
}

func RemoveCredential(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		if err := requireRecentAccount(ctx, tx, p); err != nil {
			return err
		}
		current, err := access.SessionPrincipal(ctx, tx, p.SessionID, uuid.Nil())
		if err != nil {
			return err
		}
		if current.IdentityID == id || current.PasskeyID == id {
			return ErrRecentProofRequired
		}
		tag, err := tx.Exec(ctx, `UPDATE auth_identities SET revoked_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND user_id=$2 AND provider<>'email' AND revoked_at IS NULL`, id, p.UserID)
		if err != nil {
			return err
		}
		keyTag, err := tx.Exec(ctx, `UPDATE passkey_credentials SET revoked_at=clock_timestamp() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, p.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected()+keyTag.RowsAffected() != 1 {
			return ErrInvalidToken
		}
		if err := slack.RetireAccountAccess(ctx, tx, p.UserID, id); err != nil {
			return err
		}
		return accountSecurityEvent(ctx, tx, cfg, p, "identity.credential_removed", id, "")
	})
}

func UpdateAccountProfile(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, name string) error {
	if len(name) == 0 || len(name) > 200 {
		return invalidInput("display name must contain 1 to 200 bytes")
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		if err := requireRecentAccount(ctx, tx, p); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET display_name=$2,updated_at=clock_timestamp() WHERE id=$1`, p.UserID, name)
		if err != nil {
			return err
		}
		return accountSecurityEvent(ctx, tx, cfg, p, "identity.profile_updated", uuid.Nil(), "")
	})
}

func LinkProvider(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, external ExternalIdentity, rawSession string) (string, error) {
	var raw string
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "identity:"+external.Provider+":"+external.Subject); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		if err := requireRecentAccount(ctx, tx, p); err != nil {
			return err
		}
		q := db.New(tx)
		existing, err := q.GetAuthIdentity(ctx, db.GetAuthIdentityParams{Provider: external.Provider, Subject: external.Subject})
		if err == nil && nullableUUID(existing.UserID) != p.UserID {
			return ErrIdentityConflict
		}
		if err != nil && !isNoRows(err) {
			return err
		}
		added := isNoRows(err) || existing.RevokedAt.Valid
		if added {
			existing, err = q.CreateAuthIdentity(ctx, db.CreateAuthIdentityParams{ID: optionalUUID(uuid.NewV7()), UserID: optionalUUID(p.UserID), Provider: external.Provider, Subject: external.Subject})
		}
		if err != nil {
			return err
		}
		hash, err := auth.HashToken(cfg.sessionKey, rawSession)
		if err != nil {
			return ErrInvalidToken
		}
		proof := provedUser(p.UserID, nullableUUID(existing.ID), uuid.Nil(), p.AccessEpoch, time.Now(), external.Provider)
		raw, err = confirmBrowserProof(ctx, tx, cfg, p.SessionID, hash, proof)
		if err != nil {
			return err
		}
		if added {
			return accountSecurityEvent(ctx, tx, cfg, p, "identity.credential_added", nullableUUID(existing.ID), external.Provider)
		}
		return nil
	})
	return raw, err
}
