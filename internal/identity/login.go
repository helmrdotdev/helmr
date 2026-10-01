package identity

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

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

// SignIn records the external identity, creating its user on first sign-in,
// and issues a login session that selects no organization in one
// transaction. It returns the raw session token.
func SignIn(ctx context.Context, txb db.TxBeginner, cfg Config, external ExternalIdentity) (string, error) {
	var rawMachine string
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		user, err := upsertExternalIdentity(ctx, q, cfg, external)
		if err != nil {
			return err
		}
		rawMachine, err = startLoginSession(ctx, q, cfg, user, external.DisplayName, nil)
		return err
	})
	if err != nil {
		return "", err
	}
	return rawMachine, nil
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

// SignInWithInvitation accepts the invitation whose token hashes to
// invitationTokenHash for the external identity, which must verify the
// invitee email, and issues a login session for the invitation's organization.
// It returns the raw session token.
func SignInWithInvitation(ctx context.Context, txb db.TxBeginner, cfg Config, invitationTokenHash []byte, external ExternalIdentity) (string, error) {
	var rawMachine string
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		invitation, err := org.PendingInvitationByTokenHash(ctx, q, invitationTokenHash)
		if err != nil {
			return acceptanceError(err)
		}
		if !external.Verifies(invitation.InviteeEmail) {
			return ErrWrongAccount
		}
		user, err := upsertExternalIdentity(ctx, q, cfg, external)
		if err != nil {
			return err
		}
		rawMachine, err = startLoginSession(ctx, q, cfg, user, external.DisplayName, &invitation)
		return err
	})
	if err != nil {
		return "", err
	}
	return rawMachine, nil
}

// signedInUser is the user an identity upsert selected.
type signedInUser struct {
	id       pgtype.UUID
	disabled bool
}

func upsertExternalIdentity(ctx context.Context, q db.Querier, cfg Config, external ExternalIdentity) (signedInUser, error) {
	email := pgtype.Text{}
	if external.Email != "" {
		email = pgtype.Text{String: external.Email, Valid: true}
	}
	user, err := q.UpsertAuthIdentity(ctx, db.UpsertAuthIdentityParams{
		UserID:           pgvalue.UUID(uuid.NewV7()),
		IdentityID:       pgvalue.UUID(uuid.NewV7()),
		IdentityProvider: external.Provider,
		IdentitySubject:  external.Subject,
		DisplayName:      external.DisplayName,
		ProfileImageURL:  pgtype.Text{String: external.ProfileImageURL, Valid: external.ProfileImageURL != ""},
		Email:            email,
		EmailVerified:    external.EmailVerified,
		Admin:            cfg.initialAdmin(external.Email, external.EmailVerified),
	})
	if err != nil {
		return signedInUser{}, fmt.Errorf("record auth identity: %w", err)
	}
	return signedInUser{id: user.ID, disabled: user.DisabledAt.Valid}, nil
}

// startLoginSession issues a login session for an enabled user. With an
// invitation, the user first joins its organization and the session selects
// that organization.
func startLoginSession(ctx context.Context, q db.Querier, cfg Config, user signedInUser, displayName string, invitation *org.PendingInvitation) (string, error) {
	if user.disabled {
		return "", ErrInactiveMember
	}
	orgID := pgtype.UUID{}
	if invitation != nil {
		userID, err := pgvalue.UUIDValue(user.id)
		if err != nil {
			return "", fmt.Errorf("user id: %w", err)
		}
		if err := org.AcceptInvitation(ctx, q, *invitation, userID, displayName); err != nil {
			return "", acceptanceError(err)
		}
		orgID = pgvalue.UUID(invitation.OrgID)
	}
	return issueLoginSession(ctx, q, cfg, user.id, orgID)
}

// acceptanceError reports the org errors of invitation acceptance as the
// sign-in errors of this package.
func acceptanceError(err error) error {
	switch {
	case errors.Is(err, org.ErrInvitationNotFound):
		return ErrInvalidToken
	case errors.Is(err, org.ErrAlreadyMember):
		return ErrAlreadyMember
	case errors.Is(err, org.ErrUserDisabled):
		return ErrInactiveMember
	default:
		return err
	}
}

// issueLoginSession creates a login session for the user, selecting orgID when
// it is valid, and returns its raw token.
func issueLoginSession(ctx context.Context, q db.Querier, cfg Config, userID pgtype.UUID, orgID pgtype.UUID) (string, error) {
	raw, err := auth.GenerateOpaque(loginSessionTokenBytes)
	if err != nil {
		return "", fmt.Errorf("generate login session: %w", err)
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", fmt.Errorf("hash login session: %w", err)
	}
	if _, err := q.CreateAuthSession(ctx, db.CreateAuthSessionParams{
		ID:        pgvalue.UUID(uuid.NewV7()),
		OrgID:     orgID,
		UserID:    userID,
		TokenHash: hash,
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(cfg.lifetimes.Session)),
	}); err != nil {
		return "", fmt.Errorf("create login session: %w", err)
	}
	return raw, nil
}

// AuthenticateLoginSession resolves a raw login session token to its
// principal and extends the session's lifetime. An unknown, revoked or expired
// session, a disabled user, or a session whose selected membership is no
// longer active is auth.ErrUnauthenticated. A session that selects no
// organization takes the user's first active membership.
func AuthenticateLoginSession(ctx context.Context, q db.Querier, cfg Config, rawMachine string) (auth.Principal, error) {
	tokenHash, err := auth.HashToken(cfg.sessionKey, rawMachine)
	if err != nil {
		return auth.Principal{}, err
	}
	row, err := q.GetAuthSessionByTokenHash(ctx, tokenHash)
	if isNoRows(err) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("load login session: %w", err)
	}
	sessionID, err := pgvalue.UUIDValue(row.ID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("login session id: %w", err)
	}
	userID, err := pgvalue.UUIDValue(row.UserID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("login session user id: %w", err)
	}
	if err := q.RefreshAuthSession(ctx, db.RefreshAuthSessionParams{
		ID:        row.ID,
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(cfg.lifetimes.Session)),
	}); err != nil {
		return auth.Principal{}, fmt.Errorf("refresh login session: %w", err)
	}
	principal := auth.Principal{
		UserID:    userID,
		SessionID: sessionID,
		Kind:      auth.PrincipalKindSession,
		Admin:     row.Admin,
	}
	if row.OrgID.Valid {
		orgID, err := pgvalue.UUIDValue(row.OrgID)
		if err != nil {
			return auth.Principal{}, fmt.Errorf("login session org id: %w", err)
		}
		principal.OrgID = orgID
		principal.Role = auth.Role(row.Role)
	}
	return principal, nil
}

// RevokeLoginSession revokes the login session with the raw token. An empty,
// unknown or already revoked token revokes nothing.
func RevokeLoginSession(ctx context.Context, q db.Querier, cfg Config, rawMachine string) error {
	tokenHash, err := auth.HashToken(cfg.sessionKey, rawMachine)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := q.RevokeAuthSessionByTokenHash(ctx, tokenHash); err != nil {
		return fmt.Errorf("revoke login session: %w", err)
	}
	return nil
}
