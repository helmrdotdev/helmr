package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type AssertionOptions struct {
	ChallengeID uuid.UUID                                  `json:"challenge_id"`
	PublicKey   protocol.PublicKeyCredentialRequestOptions `json:"publicKey"`
}
type RegistrationOptions struct {
	ChallengeID uuid.UUID                                   `json:"challenge_id"`
	PublicKey   protocol.PublicKeyCredentialCreationOptions `json:"publicKey"`
}

type keyTarget struct {
	user, operator, session, enrollment, repair uuid.UUID
	purpose                                     string
	operatorScope                               bool
}

func storeCeremony(ctx context.Context, tx pgx.Tx, session *webauthn.SessionData, ceremony string, target keyTarget, binding []byte) (uuid.UUID, error) {
	if len(binding) != sha256.Size {
		return uuid.Nil(), ErrInvalidToken
	}
	data, err := json.Marshal(session)
	if err != nil {
		return uuid.Nil(), err
	}
	id := uuid.NewV7()
	_, err = tx.Exec(ctx, `INSERT INTO webauthn_challenges(id,ceremony,purpose,operator_scope,verification_state,browser_hash,session_id,operator_passkey_enrollment_id,account_repair_enrollment_id,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, ceremony, target.purpose, target.operatorScope, data, binding, optionalUUID(target.session), optionalUUID(target.enrollment), optionalUUID(target.repair), session.Expires)
	return id, err
}
func loadCeremony(ctx context.Context, tx pgx.Tx, id uuid.UUID, binding []byte, ceremony string) (webauthn.SessionData, keyTarget, error) {
	var data []byte
	var user, operator, session, enrollment, repair pgtype.UUID
	var target keyTarget
	var state webauthn.SessionData
	err := tx.QueryRow(ctx, `SELECT c.verification_state,c.purpose,c.operator_scope,COALESCE(s.user_id,r.user_id),COALESCE(s.operator_id,e.operator_id),c.session_id,c.operator_passkey_enrollment_id,c.account_repair_enrollment_id
 FROM webauthn_challenges c
 LEFT JOIN login_sessions s ON s.id=c.session_id
 LEFT JOIN operator_passkey_enrollments e ON e.id=c.operator_passkey_enrollment_id
 LEFT JOIN account_repair_enrollments r ON r.id=c.account_repair_enrollment_id
 WHERE c.id=$1 AND c.browser_hash=$2 AND c.ceremony=$3 AND c.consumed_at IS NULL AND c.revoked_at IS NULL AND c.expires_at>clock_timestamp() FOR UPDATE OF c`, id, binding, ceremony).Scan(&data, &target.purpose, &target.operatorScope, &user, &operator, &session, &enrollment, &repair)
	if isNoRows(err) {
		return state, target, ErrInvalidToken
	}
	if err != nil {
		return state, target, err
	}
	target.user, target.operator, target.session, target.enrollment, target.repair = nullableUUID(user), nullableUUID(operator), nullableUUID(session), nullableUUID(enrollment), nullableUUID(repair)
	err = json.Unmarshal(data, &state)
	return state, target, err
}

// loadKeyUser locks the account or operator that owns passkeys and loads its
// live keys.
func loadKeyUser(ctx context.Context, tx pgx.Tx, userID, operatorID uuid.UUID) (passkeyUser, error) {
	var user passkeyUser
	var err error
	if operatorID != uuid.Nil() {
		err = tx.QueryRow(ctx, `SELECT webauthn_user_handle,label FROM platform_operators WHERE id=$1 FOR UPDATE`, operatorID).Scan(&user.handle, &user.name)
	} else {
		err = tx.QueryRow(ctx, `SELECT webauthn_user_handle,display_name FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&user.handle, &user.name)
	}
	if isNoRows(err) {
		return user, ErrInvalidToken
	}
	if err != nil {
		return user, err
	}
	rows, err := tx.Query(ctx, `SELECT credential_data FROM passkey_credentials WHERE revoked_at IS NULL AND (user_id=$1 OR operator_id=$2) ORDER BY id`, optionalUUID(userID), optionalUUID(operatorID))
	if err != nil {
		return user, err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return user, err
		}
		var key webauthn.Credential
		if err = json.Unmarshal(data, &key); err != nil {
			return user, err
		}
		user.credentials = append(user.credentials, key)
	}
	return user, rows.Err()
}

// BeginPasskeyLogin starts a sign-in, or a confirmation of the existing
// session when there is one.
func BeginPasskeyLogin(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, operator bool, binding []byte, existing auth.Principal) (AssertionOptions, error) {
	if wa == nil {
		return AssertionOptions{}, ErrInvalidToken
	}
	options, state, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return AssertionOptions{}, err
	}
	target := keyTarget{purpose: "login", operatorScope: operator}
	if existing.SessionID != uuid.Nil() {
		target = keyTarget{purpose: "confirm", operatorScope: operator, session: existing.SessionID}
	}
	var id uuid.UUID
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var e error
		id, e = storeCeremony(ctx, tx, state, "assert", target, binding)
		return e
	})
	return AssertionOptions{ChallengeID: id, PublicKey: options.Response}, err
}

func FinishPasskeyLogin(ctx context.Context, database db.TxBeginner, cfg Config, wa *webauthn.WebAuthn, operator bool, id uuid.UUID, binding, credentialJSON []byte, rawSession string) (result string, resultErr error) {
	action := "identity.sign_in"
	if operator {
		action = "operator.sign_in"
	}
	defer recordAuthenticationFailure(ctx, database, cfg, audit.Event{Evidence: audit.Unauthenticated("failed", "passkey"), Action: action, TargetKind: "login_session"}, &resultErr)
	if wa == nil {
		return "", ErrInvalidToken
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credentialJSON)
	if err != nil {
		return "", ErrInvalidToken
	}
	var raw string
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		// Lock the credential owner before the challenge, matching credential revocation.
		var keyID uuid.UUID
		var userID, operatorID pgtype.UUID
		err := tx.QueryRow(ctx, `SELECT id,user_id,operator_id FROM passkey_credentials WHERE credential_id=$1 AND revoked_at IS NULL`, parsed.RawID).Scan(&keyID, &userID, &operatorID)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if operator != operatorID.Valid {
			return ErrInvalidToken
		}
		user, err := loadKeyUser(ctx, tx, nullableUUID(userID), nullableUUID(operatorID))
		if err != nil {
			return err
		}
		state, target, err := loadCeremony(ctx, tx, id, binding, "assert")
		if err != nil {
			return err
		}
		if (target.purpose != "login" && target.purpose != "confirm") || target.operatorScope != operator {
			return ErrInvalidToken
		}
		if target.purpose == "confirm" && (target.user != nullableUUID(userID) || target.operator != nullableUUID(operatorID)) {
			return ErrWrongAccount
		}
		if target.purpose == "login" && rawSession != "" {
			var live bool
			if operator {
				prior, _, readErr := AuthenticateOperator(ctx, tx, cfg, rawSession)
				if readErr != nil && !errors.Is(readErr, auth.ErrUnauthenticated) {
					return readErr
				}
				live = readErr == nil && prior.SessionID != uuid.Nil()
			} else {
				prior, readErr := liveBrowserSession(ctx, tx, cfg, rawSession)
				if readErr != nil {
					return readErr
				}
				live = prior.SessionID != uuid.Nil()
			}
			if live {
				return ErrWrongAccount
			}
		}

		key, err := wa.ValidateDiscoverableLogin(func(rawID, handle []byte) (webauthn.User, error) {
			if !bytes.Equal(rawID, parsed.RawID) || !bytes.Equal(handle, user.handle) {
				return nil, ErrInvalidToken
			}
			return user, nil
		}, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		if key.Authenticator.CloneWarning && !key.Flags.BackupEligible {
			return ErrInvalidToken
		}
		if err = updateKey(ctx, tx, keyID, key); err != nil {
			return err
		}
		var at time.Time
		if err = tx.QueryRow(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1 RETURNING consumed_at`, id).Scan(&at); err != nil {
			return err
		}
		if operator {
			if target.purpose == "confirm" {
				raw, err = confirmOperatorSession(ctx, tx, cfg, target.session, rawSession, nullableUUID(operatorID), keyID, at)
			} else {
				raw, err = issueOperatorSession(ctx, tx, cfg, nullableUUID(operatorID), keyID, at)
			}
		} else {
			proved := provedUser(nullableUUID(userID), uuid.Nil(), keyID, at, "passkey")
			if target.purpose == "confirm" {
				hash, e := auth.HashToken(cfg.sessionKey, rawSession)
				if e != nil {
					return ErrInvalidToken
				}
				raw, err = confirmBrowserProof(ctx, tx, cfg, target.session, hash, proved)
			} else {
				raw, err = issueLoginSession(ctx, tx, cfg, proved)
			}
		}
		return err
	})
	return raw, err
}

func BeginAccountPasskey(ctx context.Context, database db.TxBeginner, wa *webauthn.WebAuthn, p auth.Principal, binding []byte) (RegistrationOptions, error) {
	var result RegistrationOptions
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		user, err := loadKeyUser(ctx, tx, p.UserID, uuid.Nil())
		if err != nil {
			return err
		}
		if err = requireRecentAccount(ctx, tx, p); err != nil {
			return err
		}
		options, state, err := wa.BeginRegistration(user)
		if err != nil {
			return err
		}
		id, err := storeCeremony(ctx, tx, state, "register", keyTarget{session: p.SessionID, purpose: "register"}, binding)
		result = RegistrationOptions{ChallengeID: id, PublicKey: options.Response}
		return err
	})
	return result, err
}
func FinishAccountPasskey(ctx context.Context, database db.TxBeginner, cfg Config, wa *webauthn.WebAuthn, p auth.Principal, id uuid.UUID, binding, credentialJSON []byte, label string) error {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credentialJSON)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		user, err := loadKeyUser(ctx, tx, p.UserID, uuid.Nil())
		if err != nil {
			return err
		}
		if err = requireRecentAccount(ctx, tx, p); err != nil {
			return err
		}
		state, target, err := loadCeremony(ctx, tx, id, binding, "register")
		if err != nil {
			return err
		}
		if target.purpose != "register" || target.operatorScope || target.user != p.UserID || target.session != p.SessionID {
			return ErrInvalidToken
		}
		key, err := wa.CreateCredential(user, state, parsed)
		if err != nil {
			return ErrInvalidToken
		}
		keyID, err := insertKey(ctx, tx, p.UserID, uuid.Nil(), label, key)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE webauthn_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, id)
		if err != nil {
			return err
		}
		return accountSecurityEvent(ctx, tx, cfg, p, "identity.credential_added", keyID, "passkey")
	})
}
func insertKey(ctx context.Context, tx pgx.Tx, user, operator uuid.UUID, label string, key *webauthn.Credential) (uuid.UUID, error) {
	if label == "" || len(label) > 200 {
		return uuid.Nil(), invalidInput("credential label must contain 1 to 200 bytes")
	}
	data, err := json.Marshal(key)
	if err != nil {
		return uuid.Nil(), err
	}
	id := uuid.NewV7()
	_, err = tx.Exec(ctx, `INSERT INTO passkey_credentials(id,credential_id,user_id,operator_id,label,credential_data) VALUES($1,$2,$3,$4,$5,$6)`, id, key.ID, optionalUUID(user), optionalUUID(operator), label, data)
	return id, err
}
func updateKey(ctx context.Context, tx pgx.Tx, id uuid.UUID, key *webauthn.Credential) error {
	data, err := json.Marshal(key)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE passkey_credentials SET credential_data=$2,last_used_at=clock_timestamp() WHERE id=$1 AND revoked_at IS NULL`, id, data)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInvalidToken
	}
	return nil
}
