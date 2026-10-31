package identity

import (
	"context"
	"crypto/sha256"
	"time"
	"uuid"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// lockOperatorEnrollment locks the operator before its enrollment, matching
// every other change to the operator's keys and sessions.
func lockOperatorEnrollment(ctx context.Context, tx pgx.Tx, id uuid.UUID, binding []byte) (uuid.UUID, time.Time, passkeyUser, error) {
	var operator uuid.UUID
	var expires time.Time
	var user passkeyUser
	if len(binding) != sha256.Size {
		return operator, expires, user, ErrInvalidToken
	}
	if err := tx.QueryRow(ctx, `SELECT operator_id FROM operator_passkey_enrollments WHERE id=$1 AND browser_hash=$2`, id, binding).Scan(&operator); err != nil {
		return operator, expires, user, ErrInvalidToken
	}
	user, err := loadKeyUser(ctx, tx, uuid.Nil(), operator)
	if err != nil {
		return operator, expires, user, err
	}
	err = tx.QueryRow(ctx, `SELECT expires_at FROM operator_passkey_enrollments WHERE id=$1 AND browser_hash=$2 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, id, binding).Scan(&expires)
	if isNoRows(err) {
		return operator, expires, user, ErrInvalidToken
	}
	return operator, expires, user, err
}

func BeginOperatorRegistration(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, id uuid.UUID, binding []byte) (RegistrationOptions, error) {
	var result RegistrationOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		_, expires, user, err := lockOperatorEnrollment(ctx, tx, id, binding)
		if err != nil {
			return err
		}
		options, state, err := wa.BeginRegistration(user)
		if err != nil {
			return err
		}
		if state.Expires.After(expires) {
			state.Expires = expires
		}
		challenge, err := storeCeremony(ctx, tx, state, "register", keyTarget{enrollment: id, purpose: "enroll", operatorScope: true}, binding)
		result = RegistrationOptions{ChallengeID: challenge, PublicKey: options.Response}
		return err
	})
	return result, err
}

// CompleteOperatorRegistration installs the registered key as the operator's
// only key and signs out every operator session. For a new operator there is
// nothing to retire. A key that turns out unusable is replaced the same way,
// with a new capability from the deployment.
func CompleteOperatorRegistration(ctx context.Context, database db.TxBeginner, cfg Config, wa *webauthn.WebAuthn, id, challenge uuid.UUID, binding, response []byte, label string) error {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		operator, _, user, err := lockOperatorEnrollment(ctx, tx, id, binding)
		if err != nil {
			return err
		}
		state, target, err := loadCeremony(ctx, tx, challenge, binding, "register")
		if err != nil {
			return err
		}
		if target.enrollment != id || target.operator != operator || target.purpose != "enroll" {
			return ErrInvalidToken
		}
		key, err := wa.CreateCredential(user, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		repaired := len(user.credentials) > 0
		// Insert first: a global credential conflict must not retire the old keys.
		newID, err := insertKey(ctx, tx, uuid.Nil(), operator, label, key)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE passkey_credentials SET revoked_at=clock_timestamp() WHERE operator_id=$1 AND id<>$2 AND revoked_at IS NULL`, operator, newID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE operator_id=$1 AND revoked_at IS NULL`, operator); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE operator_passkey_enrollments SET consumed_at=CASE WHEN id=$2 THEN clock_timestamp() ELSE consumed_at END,revoked_at=CASE WHEN id<>$2 THEN clock_timestamp() ELSE revoked_at END WHERE operator_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, operator, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, challenge); err != nil {
			return err
		}
		action := "operator.credential_added"
		if repaired {
			action = "operator.repaired"
		}
		return operatorSecurityEvent(ctx, tx, cfg, auth.Principal{Kind: auth.PrincipalKindOperator, OperatorID: operator, PasskeyID: newID}, action, newID)
	})
}
