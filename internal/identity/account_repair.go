package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"time"
	"uuid"
)

// IssueAccountRepair is only called by the deployment-local self-host command.
func IssueAccountRepair(ctx context.Context, database db.TxBeginner, user, operator uuid.UUID) (OperatorEnrollment, error) {
	var result OperatorEnrollment
	if user == uuid.Nil() || operator == uuid.Nil() {
		return result, ErrInvalidToken
	}
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		return result, err
	}
	hash := sha256.Sum256([]byte(raw))
	result.ID = uuid.NewV7()
	result.Capability = raw
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, _, err := loadKeyUser(ctx, tx, uuid.Nil(), operator); err != nil {
			return err
		}
		_, epoch, err := loadKeyUser(ctx, tx, user, uuid.Nil())
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE account_repair_enrollments SET revoked_at=clock_timestamp(),provisional_credential=NULL WHERE user_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, user); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO account_repair_enrollments(id,user_id,operator_id,capability_hash,expected_access_epoch,expires_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '15 minutes') RETURNING expires_at`, result.ID, user, operator, hash[:], epoch).Scan(&result.ExpiresAt)
	})
	return result, err
}
func currentRepairOperator(ctx context.Context, tx pgx.Tx, p auth.Principal) error {
	if p.OperatorID == uuid.Nil() {
		return ErrInvalidToken
	}
	if _, _, err := loadKeyUser(ctx, tx, uuid.Nil(), p.OperatorID); err != nil {
		return err
	}
	current, err := operatorFromSession(ctx, tx, p.SessionID)
	if err != nil {
		return err
	}
	if current.OperatorID != p.OperatorID || current.AccessEpoch != p.AccessEpoch || time.Since(current.AuthenticatedAt) > time.Hour {
		return ErrRecentProofRequired
	}
	return nil
}
func RedeemAccountRepair(ctx context.Context, database db.TxBeginner, p auth.Principal, raw string, binding []byte) (OperatorEnrollment, error) {
	var result OperatorEnrollment
	if len(binding) != sha256.Size || len(raw) < 40 {
		return result, ErrInvalidToken
	}
	hash := sha256.Sum256([]byte(raw))
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := currentRepairOperator(ctx, tx, p); err != nil {
			return err
		}
		var user uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM account_repair_enrollments WHERE capability_hash=$1 AND operator_id=$2`, hash[:], p.OperatorID).Scan(&user); err != nil {
			return ErrInvalidToken
		}
		_, epoch, err := loadKeyUser(ctx, tx, user, uuid.Nil())
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE account_repair_enrollments SET browser_hash=$3 WHERE capability_hash=$1 AND operator_id=$2 AND expected_access_epoch=$4 AND browser_hash IS NULL AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() RETURNING id,expires_at`, hash[:], p.OperatorID, binding, epoch).Scan(&result.ID, &result.ExpiresAt)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		return err
	})
	return result, err
}

type accountRepair struct {
	id, user    uuid.UUID
	epoch       int64
	provisional []byte
	expires     time.Time
}

func lockAccountRepair(ctx context.Context, tx pgx.Tx, p auth.Principal, id uuid.UUID, binding []byte) (accountRepair, passkeyUser, error) {
	result := accountRepair{id: id}
	var user passkeyUser
	if err := currentRepairOperator(ctx, tx, p); err != nil {
		return result, user, err
	}
	if err := tx.QueryRow(ctx, `SELECT user_id FROM account_repair_enrollments WHERE id=$1 AND operator_id=$2 AND browser_hash=$3`, id, p.OperatorID, binding).Scan(&result.user); err != nil {
		return result, user, ErrInvalidToken
	}
	user, epoch, err := loadKeyUser(ctx, tx, result.user, uuid.Nil())
	if err != nil {
		return result, user, err
	}
	err = tx.QueryRow(ctx, `SELECT expected_access_epoch,provisional_credential,expires_at FROM account_repair_enrollments WHERE id=$1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, id).Scan(&result.epoch, &result.provisional, &result.expires)
	if isNoRows(err) {
		return result, user, ErrInvalidToken
	}
	if err != nil {
		return result, user, err
	}
	if epoch != result.epoch {
		return result, user, ErrInvalidToken
	}
	return result, user, nil
}
func BeginAccountRepairRegistration(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, p auth.Principal, id uuid.UUID, binding []byte) (RegistrationOptions, error) {
	var result RegistrationOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		repair, user, err := lockAccountRepair(ctx, tx, p, id, binding)
		if err != nil {
			return err
		}
		if len(repair.provisional) > 0 {
			return ErrEnrollmentConflict
		}
		options, state, err := wa.BeginRegistration(user)
		if err != nil {
			return err
		}
		if state.Expires.After(repair.expires) {
			state.Expires = repair.expires
		}
		challenge, err := storeCeremony(ctx, tx, state, "register", keyTarget{user: repair.user, repair: id, epoch: repair.epoch, purpose: "repair"}, binding)
		result = RegistrationOptions{ChallengeID: challenge, PublicKey: options.Response}
		return err
	})
	return result, err
}
func CompleteAccountRepairRegistration(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, p auth.Principal, id, challenge uuid.UUID, binding, response []byte) error {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		repair, user, err := lockAccountRepair(ctx, tx, p, id, binding)
		if err != nil {
			return err
		}
		if len(repair.provisional) > 0 {
			return ErrEnrollmentConflict
		}
		state, target, err := loadCeremony(ctx, tx, challenge, binding, "register")
		if err != nil {
			return err
		}
		if target.purpose != "repair" || target.repair != id || target.user != repair.user || target.epoch != repair.epoch {
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
		if _, err = tx.Exec(ctx, `UPDATE account_repair_enrollments SET provisional_credential=$2 WHERE id=$1 AND provisional_credential IS NULL`, id, data); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, challenge)
		return err
	})
}
func BeginAccountRepairAssertion(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, p auth.Principal, id uuid.UUID, binding []byte) (AssertionOptions, error) {
	var result AssertionOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		repair, user, err := lockAccountRepair(ctx, tx, p, id, binding)
		if err != nil {
			return err
		}
		if len(repair.provisional) == 0 {
			return ErrEnrollmentConflict
		}
		var key webauthn.Credential
		if err = json.Unmarshal(repair.provisional, &key); err != nil {
			return err
		}
		user.credentials = []webauthn.Credential{key}
		options, state, err := wa.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return err
		}
		if state.Expires.After(repair.expires) {
			state.Expires = repair.expires
		}
		challenge, err := storeCeremony(ctx, tx, state, "assert", keyTarget{user: repair.user, repair: id, epoch: repair.epoch, purpose: "repair"}, binding)
		result = AssertionOptions{ChallengeID: challenge, PublicKey: options.Response}
		return err
	})
	return result, err
}
func VerifyAccountRepair(ctx context.Context, database db.TxBeginner, cfg Config, wa *webauthn.WebAuthn, p auth.Principal, id, challenge uuid.UUID, binding, response []byte, label string) error {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		repair, user, err := lockAccountRepair(ctx, tx, p, id, binding)
		if err != nil {
			return err
		}
		if len(repair.provisional) == 0 {
			return ErrEnrollmentConflict
		}
		var staged webauthn.Credential
		if err = json.Unmarshal(repair.provisional, &staged); err != nil {
			return err
		}
		user.credentials = []webauthn.Credential{staged}
		state, target, err := loadCeremony(ctx, tx, challenge, binding, "assert")
		if err != nil {
			return err
		}
		if target.purpose != "repair" || target.repair != id || target.user != repair.user || target.epoch != repair.epoch {
			return ErrInvalidToken
		}
		key, err := wa.ValidateLogin(user, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		if !bytes.Equal(key.ID, staged.ID) || !bytes.Equal(key.PublicKey, staged.PublicKey) {
			return ErrInvalidToken
		}
		keyID, err := insertKey(ctx, tx, repair.user, uuid.Nil(), label, key)
		if err != nil {
			return err
		}
		if err = invalidateAccount(ctx, tx, repair.user); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE account_repair_enrollments SET consumed_at=CASE WHEN id=$2 THEN clock_timestamp() ELSE consumed_at END,revoked_at=CASE WHEN id<>$2 THEN clock_timestamp() ELSE revoked_at END,provisional_credential=NULL WHERE user_id=$1 AND consumed_at IS NULL`, repair.user, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, challenge)
		if err != nil {
			return err
		}
		if err = audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: audit.FromPrincipal(p), Action: "identity.account_repaired", TargetKind: "user", TargetID: repair.user, TargetUserID: repair.user, Details: audit.IdentitySecurityDetails{CredentialID: keyID, Method: "passkey"}}); err != nil {
			return err
		}
		var recipient string
		if err = tx.QueryRow(ctx, `SELECT COALESCE(primary_email,'') FROM users WHERE id=$1`, repair.user).Scan(&recipient); err != nil {
			return err
		}
		return enqueueSecurityNotification(ctx, tx, repair.user, "identity.account_repaired", recipient)
	})
}
