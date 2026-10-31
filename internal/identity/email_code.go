package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type EmailChallenge struct {
	ID          uuid.UUID `json:"challenge_id"`
	Email       string    `json:"-"`
	Code        string    `json:"-"`
	ExpiresAt   time.Time `json:"expires_at"`
	ResendAfter time.Time `json:"resend_after"`
	Generation  int64     `json:"-"`
}

type EmailStart struct {
	Email, Next  string
	InvitationID uuid.UUID
	BrowserHash  []byte
	Session      auth.Principal
	EmailChange  bool
}

func codeMAC(cfg Config, id uuid.UUID, generation int64, code string) []byte {
	h := hmac.New(sha256.New, cfg.emailCodeKey)
	fmt.Fprintf(h, "email-code:%s:%d:%s", id, generation, code)
	return h.Sum(nil)
}
func newEmailCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%08d", n.Int64()), nil
}

func StartEmailChallenge(ctx context.Context, database db.TxBeginner, cfg Config, input EmailStart) (EmailChallenge, error) {
	email, err := org.NormalizeEmail(input.Email)
	if err != nil {
		return EmailChallenge{}, err
	}
	if len(input.BrowserHash) != sha256.Size {
		return EmailChallenge{}, ErrInvalidToken
	}
	purpose := "login"
	if input.EmailChange {
		purpose = "email_change"
	}
	result := EmailChallenge{ID: uuid.NewV7(), Email: email}
	result.Code, err = newEmailCode()
	if err != nil {
		return result, err
	}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "email:"+email); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM email_challenges WHERE email=$1 AND created_at>clock_timestamp()-interval '15 minutes'`, email).Scan(&count); err != nil {
			return err
		}
		if count >= 5 {
			return ErrRateLimited
		}
		if input.EmailChange {
			if err := requireRecentAccount(ctx, tx, input.Session); err != nil {
				return err
			}
		}
		if input.InvitationID != uuid.Nil() {
			var matches bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM invitations WHERE id=$1 AND lower(invitee_email)=$2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp())`, input.InvitationID, email).Scan(&matches); err != nil {
				return err
			}
			if !matches {
				return ErrInvalidToken
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO email_challenges(id,email,purpose,code_mac,next_send_at,browser_hash,session_id,safe_next,expires_at)
  VALUES($1,$2,$3,$4,clock_timestamp()+interval '60 seconds',$5,$6,$7,clock_timestamp()+interval '10 minutes') RETURNING expires_at,next_send_at`, result.ID, email, purpose, codeMAC(cfg, result.ID, 0, result.Code), input.BrowserHash, optionalUUID(input.Session.SessionID), input.Next).Scan(&result.ExpiresAt, &result.ResendAfter); err != nil {
			return err
		}
		return audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: audit.Unauthenticated("absent", ""), Action: "identity.email_code_requested", TargetKind: "email_challenge", TargetID: result.ID, Details: audit.AuthenticationDetails{Method: "email_code"}})
	})
	return result, err
}

func MarkEmailDelivered(ctx context.Context, database db.DBTX, c EmailChallenge) error {
	_, err := database.Exec(ctx, `UPDATE email_challenges SET sent_at=clock_timestamp() WHERE id=$1 AND generation=$2 AND consumed_at IS NULL AND revoked_at IS NULL`, c.ID, c.Generation)
	return err
}

func ResendEmailChallenge(ctx context.Context, database db.TxBeginner, cfg Config, id uuid.UUID, binding []byte, purpose string) (EmailChallenge, error) {
	result := EmailChallenge{ID: id}
	code, err := newEmailCode()
	if err != nil {
		return result, err
	}
	result.Code = code
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var generation int64
		err := tx.QueryRow(ctx, `SELECT email,generation,expires_at,next_send_at FROM email_challenges WHERE id=$1 AND browser_hash=$2 AND purpose=$3 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, id, binding, purpose).Scan(&result.Email, &generation, &result.ExpiresAt, &result.ResendAfter)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if time.Now().Before(result.ResendAfter) || generation >= 4 {
			return ErrRateLimited
		}
		result.Generation = generation + 1
		if err := tx.QueryRow(ctx, `UPDATE email_challenges SET generation=$2,code_mac=$3,sent_at=NULL,next_send_at=clock_timestamp()+interval '60 seconds' WHERE id=$1 RETURNING next_send_at`, id, result.Generation, codeMAC(cfg, id, result.Generation, code)).Scan(&result.ResendAfter); err != nil {
			return err
		}
		return audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: audit.Unauthenticated("absent", ""), Action: "identity.email_code_requested", TargetKind: "email_challenge", TargetID: id, Details: audit.AuthenticationDetails{Method: "email_code"}})
	})
	return result, err
}

type EmailCompletion struct{ Token, Next string }

// CompleteEmailChallenge persists failed attempts even when verification fails.
// The successful transaction consumes the challenge and authenticates exactly
// its proved account; an invitation remains a separate consent operation.
func CompleteEmailChallenge(ctx context.Context, database db.TxBeginner, cfg Config, id uuid.UUID, binding []byte, code, rawSession, purpose string) (completion EmailCompletion, resultErr error) {
	defer recordAuthenticationFailure(ctx, database, cfg, audit.Event{Evidence: audit.Unauthenticated("failed", "email_code"), Action: "identity.sign_in", TargetKind: "login_session"}, &resultErr)
	if id == uuid.Nil() || len(binding) != sha256.Size {
		return EmailCompletion{}, ErrInvalidToken
	}
	var result EmailCompletion
	var rejected error
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var email, next string
		var generation int64
		var attempts int
		var mac []byte
		var userID, sessionID pgtype.UUID
		// Resolve immutable email first, then lock account before protocol state. This
		// matches revoke-all; the locked challenge is still fully rechecked below.
		var ownerEmail string
		if err := tx.QueryRow(ctx, `SELECT email FROM email_challenges WHERE id=$1 AND browser_hash=$2`, id, binding).Scan(&ownerEmail); err != nil {
			if isNoRows(err) {
				return ErrInvalidToken
			}
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "email:"+ownerEmail); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE lower(primary_email)=$1 OR id=(SELECT s.user_id FROM email_challenges c JOIN login_sessions s ON s.id=c.session_id WHERE c.id=$2) ORDER BY id FOR UPDATE`, ownerEmail, id); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT c.email,c.safe_next,c.generation,c.failed_attempts,c.code_mac,s.user_id,c.session_id
 FROM email_challenges c LEFT JOIN login_sessions s ON s.id=c.session_id
 WHERE c.id=$1 AND c.browser_hash=$2 AND c.purpose=$3 AND c.consumed_at IS NULL AND c.revoked_at IS NULL AND c.sent_at IS NOT NULL AND c.expires_at>clock_timestamp() FOR UPDATE OF c`, id, binding, purpose).Scan(&email, &next, &generation, &attempts, &mac, &userID, &sessionID)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if attempts >= 5 {
			return ErrRateLimited
		}
		if !hmac.Equal(mac, codeMAC(cfg, id, generation, code)) {
			if _, err = tx.Exec(ctx, `UPDATE email_challenges SET failed_attempts=failed_attempts+1 WHERE id=$1`, id); err != nil {
				return err
			}
			rejected = ErrInvalidToken
			return nil
		}
		if purpose == "email_change" {
			token, err := completeEmailChange(ctx, tx, cfg, id, userID, sessionID, email, rawSession)
			if err != nil {
				return err
			}
			result.Token = token
			result.Next = "/settings/account"
			return nil
		}
		user, err := emailUser(ctx, tx, email)
		if err != nil {
			return err
		}
		if userID.Valid && userID != user.id {
			return ErrWrongAccount
		}
		if sessionID.Valid {
			hash, err := auth.HashToken(cfg.sessionKey, rawSession)
			if err != nil {
				return ErrInvalidToken
			}
			var live bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM login_sessions WHERE id=$1 AND user_id=$2 AND token_hash=$3 AND revoked_at IS NULL AND expires_at>clock_timestamp() AND channel='browser')`, sessionID, user.id, hash).Scan(&live); err != nil {
				return err
			}
			if !live {
				return ErrInvalidToken
			}
			result.Token, err = confirmBrowserProof(ctx, tx, cfg, pgvalue.MustUUIDValue(sessionID), hash, user)
			if err != nil {
				return err
			}
		} else {
			// A newly presented account cannot silently replace the account bound at start.
			existing, err := liveBrowserSession(ctx, tx, cfg, rawSession)
			if err != nil {
				return err
			}
			if existing.SessionID != uuid.Nil() {
				return ErrWrongAccount
			}
			result.Token, err = issueLoginSession(ctx, tx, cfg, user)
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE email_challenges SET consumed_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			return err
		}
		result.Next = next
		return nil
	})
	if err != nil {
		return EmailCompletion{}, err
	}
	if rejected != nil {
		return EmailCompletion{}, rejected
	}
	return result, nil
}

func emailUser(ctx context.Context, tx pgx.Tx, email string) (signedInUser, error) {
	q := db.New(tx)
	user, err := q.GetAccountByEmail(ctx, email)
	if isNoRows(err) {
		handle, e := randomHandle()
		if e != nil {
			return signedInUser{}, e
		}
		user, err = q.CreateAccount(ctx, db.CreateAccountParams{ID: pgvalue.UUID(uuid.NewV7()), PrimaryEmail: pgtype.Text{String: email, Valid: true}, DisplayName: email, WebauthnUserHandle: handle})
	}
	if err != nil {
		return signedInUser{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, user.ID); err != nil {
		return signedInUser{}, err
	}
	identity, err := q.GetAuthIdentity(ctx, db.GetAuthIdentityParams{Provider: "email", Subject: pgvalue.MustUUIDValue(user.ID).String()})
	if isNoRows(err) || err == nil && identity.RevokedAt.Valid {
		identity, err = q.CreateAuthIdentity(ctx, db.CreateAuthIdentityParams{ID: pgvalue.UUID(uuid.NewV7()), UserID: user.ID, Provider: "email", Subject: pgvalue.MustUUIDValue(user.ID).String()})
	}
	if err != nil {
		return signedInUser{}, err
	}
	var at time.Time
	if err = tx.QueryRow(ctx, `UPDATE auth_identities SET last_authenticated_at=clock_timestamp() WHERE id=$1 RETURNING last_authenticated_at`, identity.ID).Scan(&at); err != nil {
		return signedInUser{}, err
	}
	return provedUser(pgvalue.MustUUIDValue(user.ID), pgvalue.MustUUIDValue(identity.ID), uuid.Nil(), at, "email"), nil
}

func confirmBrowserProof(ctx context.Context, tx pgx.Tx, cfg Config, id uuid.UUID, oldHash []byte, user signedInUser) (string, error) {
	raw, err := auth.GenerateOpaque(loginSessionTokenBytes)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `UPDATE login_sessions SET token_hash=$3,identity_id=$5,passkey_id=$6,authenticated_at=$7,last_human_activity_at=clock_timestamp(),expires_at=clock_timestamp()+interval '30 days' WHERE id=$1 AND token_hash=$2 AND user_id=$4 AND channel='browser' AND revoked_at IS NULL AND expires_at>clock_timestamp()`, id, oldHash, hash, user.id, optionalUUID(user.identityID), optionalUUID(user.passkeyID), user.authenticatedAt)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", ErrInvalidToken
	}
	return raw, nil
}

// currentAccount reads the live browser session that presents this account.
func currentAccount(ctx context.Context, q db.DBTX, p auth.Principal) (auth.Principal, error) {
	if p.Channel != "browser" || p.UserID == uuid.Nil() || p.SessionID == uuid.Nil() {
		return auth.Principal{}, ErrInvalidToken
	}
	current, err := access.SessionPrincipal(ctx, q, p.SessionID, uuid.Nil())
	if err != nil {
		return auth.Principal{}, err
	}
	if current.UserID != p.UserID {
		return auth.Principal{}, ErrInvalidToken
	}
	return current, nil
}

func requireRecentAccount(ctx context.Context, q db.DBTX, p auth.Principal) error {
	current, err := currentAccount(ctx, q, p)
	if err != nil {
		return err
	}
	if !auth.ProvedRecently(current.AuthenticatedAt, time.Now()) {
		return ErrRecentProofRequired
	}
	return nil
}
