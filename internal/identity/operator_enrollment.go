package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auth"
	"time"
	"uuid"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type protectedEnrollment struct {
	id, operator uuid.UUID
	epoch        int64
	purpose      string
	provisional  []byte
	expires      time.Time
}

func lockOperatorEnrollment(ctx context.Context, tx pgx.Tx, id uuid.UUID, binding []byte) (protectedEnrollment, passkeyUser, error) {
	e := protectedEnrollment{id: id}
	var user passkeyUser
	if len(binding) != sha256.Size {
		return e, user, ErrInvalidToken
	}
	if err := tx.QueryRow(ctx, `SELECT operator_id FROM operator_passkey_enrollments WHERE id=$1 AND browser_hash=$2`, id, binding).Scan(&e.operator); err != nil {
		return e, user, ErrInvalidToken
	}
	user, epoch, err := loadKeyUser(ctx, tx, uuid.Nil(), e.operator)
	if err != nil {
		return e, user, err
	}
	err = tx.QueryRow(ctx, `SELECT expected_access_epoch,purpose,provisional_credential,expires_at FROM operator_passkey_enrollments WHERE id=$1 AND browser_hash=$2 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, id, binding).Scan(&e.epoch, &e.purpose, &e.provisional, &e.expires)
	if isNoRows(err) {
		return e, user, ErrInvalidToken
	}
	if err != nil {
		return e, user, err
	}
	if e.epoch != epoch {
		return e, user, ErrInvalidToken
	}
	if e.purpose == "add_key" {
		var issuer uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT issuer_session_id FROM operator_passkey_enrollments WHERE id=$1`, id).Scan(&issuer); err != nil {
			return e, user, err
		}
		current, err := operatorFromSession(ctx, tx, issuer)
		if err != nil {
			return e, user, err
		}
		if current.OperatorID != e.operator || current.AccessEpoch != e.epoch || time.Since(current.AuthenticatedAt) > time.Hour {
			return e, user, ErrRecentProofRequired
		}
	}
	return e, user, nil
}

type OperatorEnrollmentState struct {
	ID        uuid.UUID `json:"enrollment_id"`
	Stage     string    `json:"stage"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ReadOperatorEnrollment resumes the browser-bound flow without its spent capability.
func ReadOperatorEnrollment(ctx context.Context, database db.TxBeginner, id uuid.UUID, binding []byte) (OperatorEnrollmentState, error) {
	var result OperatorEnrollmentState
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		e, _, err := lockOperatorEnrollment(ctx, tx, id, binding)
		if err != nil {
			return err
		}
		stage := "register"
		if len(e.provisional) > 0 {
			stage = "assert"
		}
		result = OperatorEnrollmentState{ID: id, Stage: stage, ExpiresAt: e.expires}
		return nil
	})
	return result, err
}

func BeginOperatorRegistration(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, id uuid.UUID, binding []byte) (RegistrationOptions, error) {
	var result RegistrationOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		e, user, err := lockOperatorEnrollment(ctx, tx, id, binding)
		if err != nil {
			return err
		}
		if len(e.provisional) > 0 {
			return ErrEnrollmentConflict
		}
		options, state, err := wa.BeginRegistration(user)
		if err != nil {
			return err
		}
		if state.Expires.After(e.expires) {
			state.Expires = e.expires
		}
		challenge, err := storeCeremony(ctx, tx, state, "register", keyTarget{enrollment: id, purpose: "enroll", operatorScope: true}, binding)
		result = RegistrationOptions{ChallengeID: challenge, PublicKey: options.Response}
		return err
	})
	return result, err
}
func CompleteOperatorRegistration(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, id, challenge uuid.UUID, binding, response []byte) error {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		e, user, err := lockOperatorEnrollment(ctx, tx, id, binding)
		if err != nil {
			return err
		}
		if len(e.provisional) > 0 {
			return ErrEnrollmentConflict
		}
		state, target, err := loadCeremony(ctx, tx, challenge, binding, "register")
		if err != nil {
			return err
		}
		if target.enrollment != id || target.operator != e.operator || target.epoch != e.epoch || target.purpose != "enroll" {
			return ErrInvalidToken
		}
		key, err := wa.CreateCredential(user, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		data, err := json.Marshal(key)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE operator_passkey_enrollments SET provisional_credential=$2 WHERE id=$1 AND provisional_credential IS NULL`, id, data); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, challenge)
		return err
	})
}
func BeginOperatorEnrollmentAssertion(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, id uuid.UUID, binding []byte) (AssertionOptions, error) {
	var result AssertionOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		e, user, err := lockOperatorEnrollment(ctx, tx, id, binding)
		if err != nil {
			return err
		}
		if len(e.provisional) == 0 {
			return ErrEnrollmentConflict
		}
		var key webauthn.Credential
		if err = json.Unmarshal(e.provisional, &key); err != nil {
			return err
		}
		user.credentials = []webauthn.Credential{key}
		options, state, err := wa.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return err
		}
		if state.Expires.After(e.expires) {
			state.Expires = e.expires
		}
		challenge, err := storeCeremony(ctx, tx, state, "assert", keyTarget{enrollment: id, purpose: "enroll", operatorScope: true}, binding)
		result = AssertionOptions{ChallengeID: challenge, PublicKey: options.Response}
		return err
	})
	return result, err
}
func VerifyOperatorEnrollment(ctx context.Context, database db.TxBeginner, cfg Config, wa *webauthn.WebAuthn, id, challenge uuid.UUID, binding, response []byte, label string) error {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		e, user, err := lockOperatorEnrollment(ctx, tx, id, binding)
		if err != nil {
			return err
		}
		if len(e.provisional) == 0 {
			return ErrEnrollmentConflict
		}
		var provisional webauthn.Credential
		if err = json.Unmarshal(e.provisional, &provisional); err != nil {
			return err
		}
		user.credentials = []webauthn.Credential{provisional}
		state, target, err := loadCeremony(ctx, tx, challenge, binding, "assert")
		if err != nil {
			return err
		}
		if target.enrollment != id || target.operator != e.operator || target.epoch != e.epoch || target.purpose != "enroll" {
			return ErrInvalidToken
		}
		key, err := wa.ValidateLogin(user, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		if !bytes.Equal(key.ID, provisional.ID) || !bytes.Equal(key.PublicKey, provisional.PublicKey) {
			return ErrInvalidToken
		}
		// The enrollment lock freezes the staged key through assertion and install.
		// Insert first: a global credential conflict must not retire the old keys.
		newID, err := insertKey(ctx, tx, uuid.Nil(), e.operator, label, key)
		if err != nil {
			return err
		}
		if e.purpose == "repair" {
			if _, err = tx.Exec(ctx, `UPDATE platform_operators SET access_epoch=access_epoch+1,updated_at=clock_timestamp() WHERE id=$1`, e.operator); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE passkey_credentials SET revoked_at=clock_timestamp() WHERE operator_id=$1 AND id<>$2 AND revoked_at IS NULL`, e.operator, newID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE operator_id=$1 AND revoked_at IS NULL`, e.operator); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE operator_passkey_enrollments SET consumed_at=CASE WHEN id=$2 THEN clock_timestamp() ELSE consumed_at END,revoked_at=CASE WHEN id<>$2 THEN clock_timestamp() ELSE revoked_at END,provisional_credential=NULL WHERE operator_id=$1 AND consumed_at IS NULL`, e.operator, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, challenge)
		if err != nil {
			return err
		}
		action := "operator.credential_added"
		if e.purpose == "repair" {
			action = "operator.repaired"
		}
		return operatorSecurityEvent(ctx, tx, cfg, auth.Principal{Kind: auth.PrincipalKindOperator, OperatorID: e.operator, PasskeyID: newID}, action, newID)
	})
}
