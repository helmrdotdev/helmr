package identity

import (
	"context"
	"crypto/sha256"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// OperatorEnrollment is a deployment-issued capability. Its raw value is
// delivered once through a protected local file, never an argument or URL.
type OperatorEnrollment struct {
	ID         uuid.UUID `json:"enrollment_id"`
	Capability string    `json:"capability"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func IssueOperatorEnrollment(ctx context.Context, database db.TxBeginner, label string, operatorID uuid.UUID) (OperatorEnrollment, error) {
	if label == "" || len(label) > 200 {
		return OperatorEnrollment{}, invalidInput("operator label is required")
	}
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		return OperatorEnrollment{}, err
	}
	hash := sha256.Sum256([]byte(raw))
	result := OperatorEnrollment{ID: uuid.NewV7(), Capability: raw}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var epoch int64
		purpose := "repair"
		if operatorID == uuid.Nil() {
			handle, err := randomHandle()
			if err != nil {
				return err
			}
			operatorID = uuid.NewV7()
			purpose = "bootstrap"
			if _, err = tx.Exec(ctx, `INSERT INTO platform_operators(id,label,webauthn_user_handle) VALUES($1,$2,$3)`, operatorID, label, handle); err != nil {
				return err
			}
		} else if err = tx.QueryRow(ctx, `SELECT access_epoch FROM platform_operators WHERE id=$1 AND disabled_at IS NULL FOR UPDATE`, operatorID).Scan(&epoch); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE credential_enrollments SET revoked_at=clock_timestamp(),provisional_credential=NULL WHERE operator_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, operatorID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO credential_enrollments(id,operator_id,purpose,capability_hash,expected_epoch,expires_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '15 minutes') RETURNING expires_at`, result.ID, operatorID, purpose, hash[:], epoch).Scan(&result.ExpiresAt)
	})
	return result, err
}
func RedeemOperatorEnrollment(ctx context.Context, database db.TxBeginner, raw string, binding []byte) (OperatorEnrollment, error) {
	if len(binding) != sha256.Size || len(raw) < 40 {
		return OperatorEnrollment{}, ErrInvalidToken
	}
	hash := sha256.Sum256([]byte(raw))
	var result OperatorEnrollment
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE credential_enrollments e SET capability_hash=NULL,browser_hash=$2 WHERE capability_hash=$1 AND browser_hash IS NULL AND e.consumed_at IS NULL AND e.revoked_at IS NULL AND e.expires_at>clock_timestamp() AND EXISTS(SELECT 1 FROM platform_operators o WHERE o.id=e.operator_id AND o.disabled_at IS NULL AND o.access_epoch=e.expected_epoch) RETURNING e.id,e.expires_at`, hash[:], binding).Scan(&result.ID, &result.ExpiresAt)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		return err
	})
	return result, err
}
func issueOperatorSession(ctx context.Context, tx pgx.Tx, cfg Config, operator, key uuid.UUID, epoch int64, at time.Time) (string, error) {
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", err
	}
	id := uuid.NewV7()
	_, err = tx.Exec(ctx, `INSERT INTO login_sessions(id,operator_id,channel,token_hash,privileged_epoch,privileged_key_id,privileged_login_at,privileged_confirmed_at,expires_at) VALUES($1,$2,'operator',$3,$4,$5,$6::timestamptz,$6::timestamptz,$6::timestamptz+interval '12 hours')`, id, operator, hash, epoch, key, at)
	if err != nil {
		return "", err
	}
	p := auth.Principal{Kind: auth.PrincipalKindOperator, OperatorID: operator, SessionID: id}
	return raw, operatorSecurityEvent(ctx, tx, cfg, p, "operator.sign_in", key)
}
func confirmOperatorSession(ctx context.Context, tx pgx.Tx, cfg Config, id uuid.UUID, old string, operator, key uuid.UUID, epoch int64, at time.Time) (string, error) {
	oldHash, err := auth.HashToken(cfg.sessionKey, old)
	if err != nil {
		return "", ErrInvalidToken
	}
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `UPDATE login_sessions SET token_hash=$3,privileged_confirmed_at=$4,privileged_key_id=$5 WHERE id=$1 AND token_hash=$2 AND operator_id=$6 AND privileged_epoch=$7 AND channel='operator' AND revoked_at IS NULL AND expires_at>$4`, id, oldHash, hash, at, key, operator, epoch)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", ErrInvalidToken
	}
	return raw, operatorSecurityEvent(ctx, tx, cfg, auth.Principal{Kind: auth.PrincipalKindOperator, OperatorID: operator, SessionID: id}, "operator.session_confirmed", key)
}
func AuthenticateOperator(ctx context.Context, database db.DBTX, cfg Config, raw string) (auth.Principal, time.Time, error) {
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return auth.Principal{}, time.Time{}, err
	}
	p := auth.Principal{Kind: auth.PrincipalKindOperator, Channel: "operator"}
	var expiry time.Time
	err = database.QueryRow(ctx, `SELECT s.id,s.operator_id,s.privileged_key_id,s.privileged_epoch,s.privileged_confirmed_at,s.expires_at
 FROM login_sessions s JOIN platform_operators o ON o.id=s.operator_id
 JOIN passkey_credentials k ON k.id=s.privileged_key_id AND k.operator_id=o.id
 WHERE s.token_hash=$1 AND s.channel='operator' AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND o.disabled_at IS NULL AND o.access_epoch=s.privileged_epoch AND k.revoked_at IS NULL`, hash).Scan(&p.SessionID, &p.OperatorID, &p.PasskeyID, &p.AccessEpoch, &p.AuthenticatedAt, &expiry)
	if isNoRows(err) {
		return p, expiry, auth.ErrUnauthenticated
	}
	return p, expiry, err
}

func renewOperatorSession(ctx context.Context, tx pgx.Tx, cfg Config, old auth.Principal, key uuid.UUID, at time.Time) (string, error) {
	// Renewal is called only after a new assertion, never from activity refresh.
	if _, err := tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1 AND operator_id=$2`, old.SessionID, old.OperatorID); err != nil {
		return "", err
	}
	return issueOperatorSession(ctx, tx, cfg, old.OperatorID, key, old.AccessEpoch, at)
}

func operatorFromSession(ctx context.Context, tx pgx.Tx, id uuid.UUID) (auth.Principal, error) {
	p := auth.Principal{Kind: auth.PrincipalKindOperator, Channel: "operator", SessionID: id}
	err := tx.QueryRow(ctx, `SELECT s.operator_id,s.privileged_key_id,s.privileged_epoch,s.privileged_confirmed_at FROM login_sessions s JOIN platform_operators o ON o.id=s.operator_id JOIN passkey_credentials k ON k.id=s.privileged_key_id AND k.operator_id=o.id WHERE s.id=$1 AND s.channel='operator' AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() AND o.disabled_at IS NULL AND s.privileged_epoch=o.access_epoch AND k.revoked_at IS NULL FOR SHARE OF o,s,k`, id).Scan(&p.OperatorID, &p.PasskeyID, &p.AccessEpoch, &p.AuthenticatedAt)
	if isNoRows(err) {
		return p, ErrInvalidToken
	}
	return p, err
}

func RevokeOperatorSession(ctx context.Context, database db.TxBeginner, cfg Config, raw string) error {
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return nil
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {

		p := auth.Principal{Kind: auth.PrincipalKindOperator}
		if err := tx.QueryRow(ctx, `SELECT operator_id FROM login_sessions WHERE token_hash=$1 AND channel='operator'`, hash).Scan(&p.OperatorID); isNoRows(err) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM platform_operators WHERE id=$1 FOR UPDATE`, p.OperatorID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1 AND channel='operator' AND revoked_at IS NULL RETURNING id,operator_id,privileged_key_id`, hash).Scan(&p.SessionID, &p.OperatorID, &p.PasskeyID)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return operatorSecurityEvent(ctx, tx, cfg, p, "operator.session_revoked", p.PasskeyID)
	})
}

func BeginOperatorKeyEnrollment(ctx context.Context, database db.TxBeginner, p auth.Principal, binding []byte) (OperatorEnrollment, error) {
	var result OperatorEnrollment
	if len(binding) != sha256.Size {
		return result, ErrInvalidToken
	}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, _, err := loadKeyUser(ctx, tx, uuid.Nil(), p.OperatorID); err != nil {
			return err
		}
		current, err := operatorFromSession(ctx, tx, p.SessionID)
		if err != nil {
			return err
		}
		if current.OperatorID != p.OperatorID || time.Since(current.AuthenticatedAt) > time.Hour {
			return ErrRecentProofRequired
		}
		result.ID = uuid.NewV7()
		return tx.QueryRow(ctx, `INSERT INTO credential_enrollments(id,operator_id,purpose,expected_epoch,issuer_session_id,browser_hash,expires_at) VALUES($1,$2,'add_key',$3,$4,$5,clock_timestamp()+interval '15 minutes') RETURNING expires_at`, result.ID, p.OperatorID, current.AccessEpoch, p.SessionID, binding).Scan(&result.ExpiresAt)
	})
	return result, err
}
func RemoveOperatorKey(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, _, err := loadKeyUser(ctx, tx, uuid.Nil(), p.OperatorID); err != nil {
			return err
		}
		current, err := operatorFromSession(ctx, tx, p.SessionID)
		if err != nil {
			return err
		}
		if current.OperatorID != p.OperatorID || current.PasskeyID == id || time.Since(current.AuthenticatedAt) > time.Hour {
			return ErrRecentProofRequired
		}
		tag, err := tx.Exec(ctx, `UPDATE passkey_credentials SET revoked_at=clock_timestamp() WHERE id=$1 AND operator_id=$2 AND revoked_at IS NULL`, id, p.OperatorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrInvalidToken
		}
		return operatorSecurityEvent(ctx, tx, cfg, current, "operator.credential_removed", id)
	})
}
