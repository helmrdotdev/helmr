package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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

const loginSessionTokenBytes = 32

// ExternalIdentity is an identity resolved by an external identity provider.
// Email is the provider's primary address; VerifiedEmails lists every address
// the provider verified.
type ExternalIdentity struct {
	Provider        string
	Subject         string
	DisplayName     string
	ProfileImageURL string
	Email           string
	EmailVerified   bool
	VerifiedEmails  []string
}

// Verifies reports whether the identity proves ownership of email: it is the
// verified primary address or one of the verified addresses. Addresses compare
// case-insensitively, ignoring surrounding space.
func (i ExternalIdentity) Verifies(email string) bool {
	email = normalizeEmail(email)
	if email == "" {
		return false
	}
	if i.EmailVerified && normalizeEmail(i.Email) == email {
		return true
	}
	for _, verified := range i.VerifiedEmails {
		if normalizeEmail(verified) == email {
			return true
		}
	}
	return false
}

// ResolveInvitation hashes a raw invitation token and loads the pending
// invitation it names. It returns the invitation and the token hash that an
// invitation sign-in started from it carries.
func ResolveInvitation(ctx context.Context, q db.Querier, cfg Config, rawToken string) (org.PendingInvitation, []byte, error) {
	tokenHash, err := auth.HashToken(cfg.invitationKey, rawToken)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return org.PendingInvitation{}, nil, ErrInvalidToken
	}
	if err != nil {
		return org.PendingInvitation{}, nil, fmt.Errorf("hash invitation token: %w", err)
	}
	invitation, err := org.PendingInvitationByTokenHash(ctx, q, tokenHash)
	if err != nil {
		return org.PendingInvitation{}, nil, acceptanceError(err)
	}
	return invitation, tokenHash, nil
}

// signedInUser carries the actual credential proof into session issuance.
type signedInUser struct {
	actor           audit.Evidence
	authentication  audit.AuthenticationDetails
	id              pgtype.UUID
	identityID      uuid.UUID
	passkeyID       uuid.UUID
	authenticatedAt time.Time
}

func upsertExternalIdentity(ctx context.Context, tx pgx.Tx, cfg Config, external ExternalIdentity) (signedInUser, error) {
	if external.Provider == "" || external.Subject == "" {
		return signedInUser{}, ErrInvalidToken
	}
	// Serialize the stable subject, including retired lifetimes. An email collision
	// is never consent to link an unrelated provider identity.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "identity:"+external.Provider+":"+external.Subject); err != nil {
		return signedInUser{}, err
	}
	q := db.New(tx)
	existing, err := q.GetAuthIdentity(ctx, db.GetAuthIdentityParams{Provider: external.Provider, Subject: external.Subject})
	if err == nil {
		if existing.RevokedAt.Valid {
			return signedInUser{}, ErrIdentityConflict
		}
		if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, existing.UserID); err != nil {
			return signedInUser{}, err
		}
		var at time.Time
		if err = tx.QueryRow(ctx, `UPDATE auth_identities SET last_authenticated_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND revoked_at IS NULL RETURNING last_authenticated_at`, existing.ID).Scan(&at); err != nil {
			return signedInUser{}, err
		}
		return provedExternal(pgvalue.MustUUIDValue(existing.UserID), pgvalue.MustUUIDValue(existing.ID), at, external), nil
	}
	if !isNoRows(err) {
		return signedInUser{}, err
	}
	// GitHub is the configured mailbox-verifying ordinary provider. Its assertion
	// may create a new account; association with an existing account needs proof
	// from that account through the explicit linking operation.
	if external.Provider != "github" || !external.EmailVerified {
		return signedInUser{}, ErrAccountProofRequired
	}
	email, err := org.NormalizeEmail(external.Email)
	if err != nil {
		return signedInUser{}, ErrAccountProofRequired
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "email:"+email); err != nil {
		return signedInUser{}, err
	}
	if _, err = q.GetAccountByEmail(ctx, email); err == nil {
		return signedInUser{}, ErrAccountProofRequired
	} else if !isNoRows(err) {
		return signedInUser{}, err
	}
	handle, err := randomHandle()
	if err != nil {
		return signedInUser{}, err
	}
	user, err := q.CreateAccount(ctx, db.CreateAccountParams{ID: pgvalue.UUID(uuid.NewV7()), PrimaryEmail: pgtype.Text{String: email, Valid: true}, DisplayName: external.DisplayName, ProfileImageURL: pgtype.Text{String: external.ProfileImageURL, Valid: external.ProfileImageURL != ""}, WebauthnUserHandle: handle})
	if err != nil {
		return signedInUser{}, err
	}
	credential, err := q.CreateAuthIdentity(ctx, db.CreateAuthIdentityParams{ID: pgvalue.UUID(uuid.NewV7()), UserID: user.ID, Provider: external.Provider, Subject: external.Subject})
	if err != nil {
		return signedInUser{}, err
	}
	return provedExternal(pgvalue.MustUUIDValue(user.ID), pgvalue.MustUUIDValue(credential.ID), credential.LastAuthenticatedAt.Time, external), nil
}

func provedUser(user, identity, key uuid.UUID, at time.Time, method string) signedInUser {
	actor := audit.VerifiedLogin(user, method)
	if method == "email" || method == "passkey" {
		kind, id := "email_code", identity
		if method == "passkey" {
			kind, id = "passkey", key
		}
		actor.Authentication = audit.Authentication{Origin: "request", Result: "verified", CredentialKind: kind, CredentialID: id}
	}
	return signedInUser{id: pgvalue.UUID(user), identityID: identity, passkeyID: key, authenticatedAt: at, actor: actor, authentication: audit.AuthenticationDetails{Method: method}}
}
func randomHandle() ([]byte, error) { h := make([]byte, 32); _, err := rand.Read(h); return h, err }

func acceptanceError(err error) error {
	if errors.Is(err, org.ErrInvitationNotFound) {
		return ErrInvalidToken
	}
	if errors.Is(err, org.ErrAlreadyMember) {
		return ErrAlreadyMember
	}
	return err
}

// issueLoginSession starts a browser session that selects no organization;
// invitations are accepted separately after explicit consent.
func issueLoginSession(ctx context.Context, tx pgx.Tx, cfg Config, user signedInUser) (string, error) {
	raw, err := auth.GenerateOpaque(loginSessionTokenBytes)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", err
	}
	id := uuid.NewV7()
	if _, err = tx.Exec(ctx, `INSERT INTO login_sessions(id,user_id,channel,token_hash,identity_id,passkey_id,authenticated_at,expires_at) VALUES($1,$2,'browser',$3,$4,$5,$6,clock_timestamp()+interval '30 days')`, id, user.id, hash, optionalUUID(user.identityID), optionalUUID(user.passkeyID), user.authenticatedAt); err != nil {
		return "", err
	}
	if err = appendSessionLifecycle(ctx, tx, cfg, user.actor, pgtype.UUID{}, "identity.sign_in", id, user.authentication); err != nil {
		return "", err
	}
	return raw, nil
}
func optionalUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil() {
		return pgtype.UUID{}
	}
	return pgvalue.UUID(id)
}

func appendSessionLifecycle(ctx context.Context, tx pgx.Tx, cfg Config, actor audit.Evidence, orgID pgtype.UUID, action string, id uuid.UUID, details audit.Details) error {
	event := audit.Event{Evidence: actor, Action: action, TargetKind: "login_session", TargetID: id, Details: details}
	if err := audit.Append(ctx, tx, cfg.auditPolicy, event); err != nil {
		return err
	}
	if orgID.Valid {
		event.OrgID = pgvalue.MustUUIDValue(orgID)
		return audit.Append(ctx, tx, cfg.auditPolicy, event)
	}
	return nil
}

// AuthenticateLoginSession requires explicit target selection. It does not
// refresh inactivity on failed admission or background observation.
func AuthenticateLoginSession(ctx context.Context, database db.DBTX, cfg Config, rawSession string, target uuid.UUID) (auth.Principal, error) {
	hash, err := auth.HashToken(cfg.sessionKey, rawSession)
	if err != nil {
		return auth.Principal{}, err
	}
	row, err := db.New(database).GetLoginSessionByTokenHash(ctx, hash)
	if isNoRows(err) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, err
	}
	p, err := access.SessionPrincipal(ctx, database, pgvalue.MustUUIDValue(row.ID), target)
	if errors.Is(err, access.ErrDenied) {
		if target != uuid.Nil() && row.Channel == "browser" {
			account, accountErr := access.SessionPrincipal(ctx, database, pgvalue.MustUUIDValue(row.ID), uuid.Nil())
			if accountErr == nil {
				return account, ErrOrganizationAccessDenied
			}
			if !errors.Is(accountErr, access.ErrDenied) {
				return auth.Principal{}, accountErr
			}
		}
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return p, err
}

// liveBrowserSession treats an expired or retired cookie as anonymous. A live
// account envelope still prevents a fresh login from replacing another account.
func liveBrowserSession(ctx context.Context, q db.DBTX, cfg Config, raw string) (auth.Principal, error) {
	p, err := AuthenticateLoginSession(ctx, q, cfg, raw, uuid.Nil())
	if errors.Is(err, auth.ErrUnauthenticated) {
		return auth.Principal{}, nil
	}
	if err != nil {
		return auth.Principal{}, err
	}
	if p.Channel != "browser" {
		return auth.Principal{}, nil
	}
	return p, nil
}

// RevokeLoginSession revokes the login session with the raw token. An empty,
// unknown or already revoked token revokes nothing.
func RevokeLoginSession(ctx context.Context, database db.TxBeginner, cfg Config, rawSession string) error {
	tokenHash, err := auth.HashToken(cfg.sessionKey, rawSession)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return nil
	}
	if err != nil {
		return err
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var id, user uuid.UUID
		var orgID pgtype.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM login_sessions WHERE token_hash=$1 AND channel IN ('browser','cli')`, tokenHash).Scan(&user); isNoRows(err) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, user); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1 AND channel IN ('browser','cli') AND revoked_at IS NULL RETURNING id,user_id,org_id`, tokenHash).Scan(&id, &user, &orgID)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		actor := audit.FromPrincipal(auth.Principal{Kind: auth.PrincipalKindSession, UserID: user, SessionID: id})
		actor.Authorization = audit.NotEvaluated()
		return appendSessionLifecycle(ctx, tx, cfg, actor, orgID, "identity.session_revoked", id, audit.SessionRevokedDetails{Reason: "logout"})
	})
}

// SignInBrowserProvider confirms the browser session that started the flow, or
// signs in a browser that started it signed out. started is that session, or nil;
// the browser must still present exactly it.
func SignInBrowserProvider(ctx context.Context, database db.TxBeginner, cfg Config, external ExternalIdentity, raw string, started uuid.UUID) (string, error) {
	var result string
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		user, err := upsertExternalIdentity(ctx, tx, cfg, external)
		if err != nil {
			return err
		}
		existing, err := liveBrowserSession(ctx, tx, cfg, raw)
		if err != nil {
			return err
		}
		if existing.SessionID != started {
			return ErrWrongAccount
		}
		if existing.SessionID == uuid.Nil() {
			result, err = issueLoginSession(ctx, tx, cfg, user)
			return err
		}
		hash, err := auth.HashToken(cfg.sessionKey, raw)
		if err != nil {
			return ErrInvalidToken
		}
		var id uuid.UUID
		var owner uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id,user_id FROM login_sessions WHERE token_hash=$1 AND channel='browser' AND revoked_at IS NULL AND expires_at>clock_timestamp()`, hash).Scan(&id, &owner); err != nil {
			return ErrInvalidToken
		}
		if owner != nullableUUID(user.id) {
			return ErrWrongAccount
		}
		result, err = confirmBrowserProof(ctx, tx, cfg, id, hash, user)
		return err
	})
	return result, err
}

func provedExternal(user, identity uuid.UUID, at time.Time, external ExternalIdentity) signedInUser {
	u := provedUser(user, identity, uuid.Nil(), at, external.Provider)
	u.authentication = audit.AuthenticationDetails{Method: "provider", Provider: external.Provider, Subject: external.Subject}
	return u
}
