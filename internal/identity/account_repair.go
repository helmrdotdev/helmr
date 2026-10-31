package identity

import (
	"context"
	"crypto/sha256"
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
		if _, err := loadKeyUser(ctx, tx, uuid.Nil(), operator); err != nil {
			return err
		}
		if _, err := loadKeyUser(ctx, tx, user, uuid.Nil()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE account_repair_enrollments SET revoked_at=clock_timestamp() WHERE user_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, user); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO account_repair_enrollments(id,user_id,operator_id,capability_hash,expires_at) VALUES($1,$2,$3,$4,clock_timestamp()+interval '15 minutes') RETURNING expires_at`, result.ID, user, operator, hash[:]).Scan(&result.ExpiresAt)
	})
	return result, err
}
func RedeemAccountRepair(ctx context.Context, database db.TxBeginner, p auth.Principal, raw string, binding []byte) (OperatorEnrollment, error) {
	var result OperatorEnrollment
	if len(binding) != sha256.Size || len(raw) < 40 {
		return result, ErrInvalidToken
	}
	hash := sha256.Sum256([]byte(raw))
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := requireRecentOperator(ctx, tx, p); err != nil {
			return err
		}
		var user uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM account_repair_enrollments WHERE capability_hash=$1 AND operator_id=$2`, hash[:], p.OperatorID).Scan(&user); err != nil {
			return ErrInvalidToken
		}
		if _, err := loadKeyUser(ctx, tx, user, uuid.Nil()); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE account_repair_enrollments SET browser_hash=$3 WHERE capability_hash=$1 AND operator_id=$2 AND browser_hash IS NULL AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() RETURNING id,expires_at`, hash[:], p.OperatorID, binding).Scan(&result.ID, &result.ExpiresAt)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		return err
	})
	return result, err
}

type accountRepair struct {
	id, user uuid.UUID
	expires  time.Time
}

func lockAccountRepair(ctx context.Context, tx pgx.Tx, p auth.Principal, id uuid.UUID, binding []byte) (accountRepair, passkeyUser, error) {
	result := accountRepair{id: id}
	var user passkeyUser
	if _, err := requireRecentOperator(ctx, tx, p); err != nil {
		return result, user, err
	}
	if err := tx.QueryRow(ctx, `SELECT user_id FROM account_repair_enrollments WHERE id=$1 AND operator_id=$2 AND browser_hash=$3`, id, p.OperatorID, binding).Scan(&result.user); err != nil {
		return result, user, ErrInvalidToken
	}
	user, err := loadKeyUser(ctx, tx, result.user, uuid.Nil())
	if err != nil {
		return result, user, err
	}
	err = tx.QueryRow(ctx, `SELECT expires_at FROM account_repair_enrollments WHERE id=$1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, id).Scan(&result.expires)
	if isNoRows(err) {
		return result, user, ErrInvalidToken
	}
	return result, user, err
}

type AccountRepairState struct {
	ID        uuid.UUID `json:"enrollment_id"`
	Target    string    `json:"target"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ReadAccountRepair requires the same browser and currently authorized operator.
func ReadAccountRepair(ctx context.Context, database db.TxBeginner, p auth.Principal, id uuid.UUID, binding []byte) (AccountRepairState, error) {
	var result AccountRepairState
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		repair, _, err := lockAccountRepair(ctx, tx, p, id, binding)
		if err != nil {
			return err
		}
		var target string
		if err = tx.QueryRow(ctx, `SELECT display_name||' ('||COALESCE(primary_email,'')||', '||id::text||')' FROM users WHERE id=$1`, repair.user).Scan(&target); err != nil {
			return err
		}
		result = AccountRepairState{ID: id, Target: target, ExpiresAt: repair.expires}
		return nil
	})
	return result, err
}

func BeginAccountRepairRegistration(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, p auth.Principal, id uuid.UUID, binding []byte) (RegistrationOptions, error) {
	var result RegistrationOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		repair, user, err := lockAccountRepair(ctx, tx, p, id, binding)
		if err != nil {
			return err
		}
		options, state, err := wa.BeginRegistration(user)
		if err != nil {
			return err
		}
		if state.Expires.After(repair.expires) {
			state.Expires = repair.expires
		}
		challenge, err := storeCeremony(ctx, tx, state, "register", keyTarget{repair: id, purpose: "repair"}, binding)
		result = RegistrationOptions{ChallengeID: challenge, PublicKey: options.Response}
		return err
	})
	return result, err
}
func CompleteAccountRepairRegistration(ctx context.Context, database db.TxBeginner, cfg Config, wa *webauthn.WebAuthn, p auth.Principal, id, challenge uuid.UUID, binding, response []byte, label string) error {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		repair, user, err := lockAccountRepair(ctx, tx, p, id, binding)
		if err != nil {
			return err
		}
		state, target, err := loadCeremony(ctx, tx, challenge, binding, "register")
		if err != nil {
			return err
		}
		if target.purpose != "repair" || target.repair != id || target.user != repair.user {
			return ErrInvalidToken
		}
		key, err := wa.CreateCredential(user, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		keyID, err := insertKey(ctx, tx, repair.user, uuid.Nil(), label, key)
		if err != nil {
			return err
		}
		// Consume this repair first so retiring the account's channels revokes
		// only the other pending repairs.
		if _, err = tx.Exec(ctx, `UPDATE account_repair_enrollments SET consumed_at=CASE WHEN id=$2 THEN clock_timestamp() ELSE consumed_at END,revoked_at=CASE WHEN id<>$2 THEN clock_timestamp() ELSE revoked_at END WHERE user_id=$1 AND consumed_at IS NULL`, repair.user, id); err != nil {
			return err
		}
		if err = retireAccountChannels(ctx, tx, repair.user, uuid.Nil()); err != nil {
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
		return enqueueSecurityNotification(ctx, tx, "identity.account_repaired", recipient)
	})
}
