package identity

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"
	"uuid"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// OperatorEnrollment is a deployment-issued capability. Its raw value is
// delivered once through a protected local file, never an argument or URL.
type OperatorEnrollment struct {
	ID         uuid.UUID `json:"enrollment_id"`
	Capability string    `json:"capability"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// IssueOperatorEnrollment issues a capability that installs a new operator key.
// A zero operatorID creates a new operator named label. Otherwise it repairs
// that operator, which keeps its name, so label must be empty.
func IssueOperatorEnrollment(ctx context.Context, database db.TxBeginner, label string, operatorID uuid.UUID) (OperatorEnrollment, error) {
	if operatorID == uuid.Nil() && (label == "" || len(label) > 200) {
		return OperatorEnrollment{}, invalidInput("operator label is required")
	}
	if operatorID != uuid.Nil() && label != "" {
		return OperatorEnrollment{}, invalidInput("operator label is only set when the operator is created")
	}
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		return OperatorEnrollment{}, err
	}
	hash := sha256.Sum256([]byte(raw))
	result := OperatorEnrollment{ID: uuid.NewV7(), Capability: raw}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if operatorID == uuid.Nil() {
			handle, err := randomHandle()
			if err != nil {
				return err
			}
			operatorID = uuid.NewV7()
			if _, err = tx.Exec(ctx, `INSERT INTO platform_operators(id,label,webauthn_user_handle) VALUES($1,$2,$3)`, operatorID, label, handle); err != nil {
				return err
			}
		} else if err := tx.QueryRow(ctx, `SELECT id FROM platform_operators WHERE id=$1 FOR UPDATE`, operatorID).Scan(&operatorID); isNoRows(err) {
			return ErrOperatorNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE operator_passkey_enrollments SET revoked_at=clock_timestamp() WHERE operator_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, operatorID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO operator_passkey_enrollments(id,operator_id,capability_hash,expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '15 minutes') RETURNING expires_at`, result.ID, operatorID, hash[:]).Scan(&result.ExpiresAt)
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
		err := tx.QueryRow(ctx, `UPDATE operator_passkey_enrollments SET capability_hash=NULL,browser_hash=$2 WHERE capability_hash=$1 AND browser_hash IS NULL AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() RETURNING id,expires_at`, hash[:], binding).Scan(&result.ID, &result.ExpiresAt)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		return err
	})
	return result, err
}
func issueOperatorSession(ctx context.Context, tx pgx.Tx, cfg Config, operator, key uuid.UUID, at time.Time) (string, error) {
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", err
	}
	id := uuid.NewV7()
	_, err = tx.Exec(ctx, `INSERT INTO login_sessions(id,operator_id,channel,token_hash,passkey_id,issued_at,authenticated_at,expires_at) VALUES($1,$2,'operator',$3,$4,$5::timestamptz,$5::timestamptz,$5::timestamptz+interval '12 hours')`, id, operator, hash, key, at)
	if err != nil {
		return "", err
	}
	p := auth.Principal{Kind: auth.PrincipalKindOperator, OperatorID: operator, SessionID: id}
	return raw, operatorSecurityEvent(ctx, tx, cfg, p, "operator.sign_in", key)
}

// confirmOperatorSession records a new proof on the same session, rotates its
// token and gives it twelve hours from that proof.
func confirmOperatorSession(ctx context.Context, tx pgx.Tx, cfg Config, id uuid.UUID, old string, operator, key uuid.UUID, at time.Time) (string, error) {
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
	tag, err := tx.Exec(ctx, `UPDATE login_sessions SET token_hash=$3,authenticated_at=$4::timestamptz,expires_at=$4::timestamptz+interval '12 hours',passkey_id=$5
 WHERE id=$1 AND token_hash=$2 AND operator_id=$6 AND channel='operator' AND revoked_at IS NULL AND expires_at>$4::timestamptz`, id, oldHash, hash, at, key, operator)
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
	row, err := db.New(database).GetLoginSessionByTokenHash(ctx, hash)
	if isNoRows(err) || (err == nil && row.Channel != "operator") {
		return auth.Principal{}, time.Time{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, time.Time{}, err
	}
	p, expiry, err := operatorFromSession(ctx, database, pgvalue.MustUUIDValue(row.ID))
	if errors.Is(err, ErrInvalidToken) {
		return auth.Principal{}, time.Time{}, auth.ErrUnauthenticated
	}
	return p, expiry, err
}

// operatorFromSession loads a live operator session whose sign-in key is still
// live, and holds that authority through the caller's transaction.
func operatorFromSession(ctx context.Context, q db.DBTX, id uuid.UUID) (auth.Principal, time.Time, error) {
	row, err := db.New(q).GetOperatorAuthority(ctx, pgvalue.UUID(id))
	if isNoRows(err) {
		return auth.Principal{}, time.Time{}, ErrInvalidToken
	}
	if err != nil {
		return auth.Principal{}, time.Time{}, err
	}
	p := auth.Principal{Kind: auth.PrincipalKindOperator, Channel: "operator", SessionID: id, OperatorID: pgvalue.MustUUIDValue(row.OperatorID), PasskeyID: pgvalue.MustUUIDValue(row.PasskeyID), AuthenticatedAt: row.AuthenticatedAt.Time}
	return p, row.ExpiresAt.Time, nil
}

// requireRecentOperator locks the operator, then requires that p is still one of
// its live sessions and that it proved itself recently. It returns the session
// as it is now.
func requireRecentOperator(ctx context.Context, tx pgx.Tx, p auth.Principal) (auth.Principal, error) {
	if p.OperatorID == uuid.Nil() || p.SessionID == uuid.Nil() {
		return auth.Principal{}, ErrInvalidToken
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM platform_operators WHERE id=$1 FOR UPDATE`, p.OperatorID); err != nil {
		return auth.Principal{}, err
	}
	current, _, err := operatorFromSession(ctx, tx, p.SessionID)
	if err != nil {
		return auth.Principal{}, err
	}
	if current.OperatorID != p.OperatorID {
		return auth.Principal{}, ErrInvalidToken
	}
	if !auth.ProvedRecently(current.AuthenticatedAt, time.Now()) {
		return auth.Principal{}, ErrRecentProofRequired
	}
	return current, nil
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
		err := tx.QueryRow(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1 AND channel='operator' AND revoked_at IS NULL RETURNING id,operator_id,passkey_id`, hash).Scan(&p.SessionID, &p.OperatorID, &p.PasskeyID)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return operatorSecurityEvent(ctx, tx, cfg, p, "operator.session_revoked", p.PasskeyID)
	})
}

// BeginOperatorKey starts registering another key for the signed-in operator.
// The ceremony is bound to this session and browser.
func BeginOperatorKey(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, p auth.Principal, binding []byte) (RegistrationOptions, error) {
	var result RegistrationOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := requireRecentOperator(ctx, tx, p); err != nil {
			return err
		}
		user, err := loadKeyUser(ctx, tx, uuid.Nil(), p.OperatorID)
		if err != nil {
			return err
		}
		options, state, err := wa.BeginRegistration(user)
		if err != nil {
			return err
		}
		id, err := storeCeremony(ctx, tx, state, "register", keyTarget{session: p.SessionID, purpose: "register", operatorScope: true}, binding)
		result = RegistrationOptions{ChallengeID: id, PublicKey: options.Response}
		return err
	})
	return result, err
}

// FinishOperatorKey installs the key registered by this session's own ceremony.
func FinishOperatorKey(ctx context.Context, database db.TxBeginner, cfg Config, wa *webauthn.WebAuthn, p auth.Principal, id uuid.UUID, binding, credentialJSON []byte, label string) error {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credentialJSON)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		current, err := requireRecentOperator(ctx, tx, p)
		if err != nil {
			return err
		}
		user, err := loadKeyUser(ctx, tx, uuid.Nil(), p.OperatorID)
		if err != nil {
			return err
		}
		state, target, err := loadCeremony(ctx, tx, id, binding, "register")
		if err != nil {
			return err
		}
		if target.purpose != "register" || !target.operatorScope || target.operator != p.OperatorID || target.session != p.SessionID {
			return ErrInvalidToken
		}
		key, err := wa.CreateCredential(user, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		keyID, err := insertKey(ctx, tx, uuid.Nil(), p.OperatorID, label, key)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			return err
		}
		return operatorSecurityEvent(ctx, tx, cfg, current, "operator.credential_added", keyID)
	})
}

func RemoveOperatorKey(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		current, err := requireRecentOperator(ctx, tx, p)
		if err != nil {
			return err
		}
		// The key this session signed in with cannot remove itself.
		if current.PasskeyID == id {
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
