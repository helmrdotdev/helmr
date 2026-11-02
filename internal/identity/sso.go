package identity

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sso"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrSSONotEligible              = errors.New("this company identity has no membership in the organization yet")
	ErrSSOReauthenticationRequired = errors.New("sign in again at your company identity provider")
	ErrSSOIdentityConflict         = errors.New("this company identity or membership is already linked to another")
	ErrOrdinarySignInRequired      = errors.New("sign in with your email, a passkey or GitHub to continue")
)

// SSOAssertion is a validated SSO response for one connection. A SAML
// assertion ID was consumed when the response was validated.
type SSOAssertion struct {
	ConnectionID uuid.UUID
	Protocol     string
	sso.Identity
}

// SSOSignIn is the outcome of an SSO sign-in. Unbound means the identity has no
// live binding and the browser continues with first association.
type SSOSignIn struct {
	OrgID   uuid.UUID
	Session string
	Unbound bool
}

// SignInSSO adds the identity's SSO proof to the browser session that started
// the flow, or signs in a browser that started it signed out with a session
// that has no ordinary source. started is that session, or nil.
func SignInSSO(ctx context.Context, database db.TxBeginner, cfg Config, a SSOAssertion, raw string, started uuid.UUID) (SSOSignIn, error) {
	var result SSOSignIn
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		orgID, err := sso.LockActive(ctx, tx, a.ConnectionID)
		if err != nil {
			return err
		}
		result.OrgID = orgID
		// A stale IdP authentication is refused before first association too, so
		// the browser repeats it with a fresh one instead of linking it.
		if err := freshSSOTime(ctx, tx, orgID, a.AuthenticatedAt); err != nil {
			return err
		}
		b, err := sso.LiveBinding(ctx, tx, a.ConnectionID, a.Subject)
		if errors.Is(err, sso.ErrNotBound) {
			result.Unbound = true
			return nil
		}
		if err != nil {
			return err
		}
		// The member, then the binding: the order member retirement takes.
		if err := admitSSOProof(ctx, tx, b, a.AuthenticatedAt); err != nil {
			return err
		}
		if err := sso.HoldBinding(ctx, tx, b); errors.Is(err, sso.ErrNotBound) {
			result.Unbound = true
			return nil
		} else if err != nil {
			return err
		}
		existing, err := liveBrowserSession(ctx, tx, cfg, raw)
		if err != nil {
			return err
		}
		if existing.SessionID != started {
			return ErrWrongAccount
		}
		session := existing.SessionID
		if session == uuid.Nil() {
			if result.Session, session, err = issueSSOSession(ctx, tx, cfg, b.UserID); err != nil {
				return err
			}
		} else if existing.UserID != b.UserID {
			return ErrWrongAccount
		}
		return addSSOProof(ctx, tx, cfg, session, b, a)
	})
	return result, err
}

// LinkSSO binds the identity to the member's own membership from the browser
// session that started the self-link, which must still have recent proof.
func LinkSSO(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, member uuid.UUID, a SSOAssertion) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		orgID, err := sso.LockActive(ctx, tx, a.ConnectionID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		if err := requireRecentAccount(ctx, tx, p); err != nil {
			return err
		}
		var live uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM org_members WHERE id=$1 AND org_id=$2 AND user_id=$3 AND removed_at IS NULL AND suspended_at IS NULL FOR SHARE`, member, orgID, p.UserID).Scan(&live); err != nil {
			if isNoRows(err) {
				return ErrSSONotEligible
			}
			return err
		}
		return bindSSO(ctx, tx, cfg, p, orgID, member, a)
	})
}

// SSOLink is a validated identity without a binding, carried to first
// association in the browser's signed flow state.
type SSOLink struct {
	ConnectionID        uuid.UUID
	Protocol            string
	Subject             string
	Email               string
	AuthenticatedAt     time.Time
	InvitationTokenHash []byte
}

// ConfirmSSOAssociation completes first association. The browser holds recent
// ordinary proof for the account whose login email is the asserted email; the
// membership comes from the invitation the flow started with or from the
// account's existing live membership.
func ConfirmSSOAssociation(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, link SSOLink) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var err error
		if orgID, err = sso.LockActive(ctx, tx, link.ConnectionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		current, err := currentAccount(ctx, tx, p)
		if err != nil {
			return err
		}
		if !auth.ProvedRecently(current.AuthenticatedAt, time.Now()) {
			return ErrRecentProofRequired
		}
		var email pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT primary_email FROM users WHERE id=$1`, p.UserID).Scan(&email); err != nil {
			return err
		}
		if !email.Valid || normalizeEmail(email.String) != normalizeEmail(link.Email) {
			return ErrWrongAccount
		}
		member, err := ssoEligibleMember(ctx, tx, cfg, current, orgID, link.InvitationTokenHash)
		if err != nil {
			return err
		}
		return bindSSO(ctx, tx, cfg, current, orgID, member, SSOAssertion{ConnectionID: link.ConnectionID, Protocol: link.Protocol,
			Identity: sso.Identity{Subject: link.Subject, Email: link.Email, AuthenticatedAt: link.AuthenticatedAt}})
	})
	return orgID, err
}

// ssoEligibleMember resolves first-association eligibility: an unexpired
// invitation of this organization for the account's email, then an existing
// live membership.
func ssoEligibleMember(ctx context.Context, tx pgx.Tx, cfg Config, current auth.Principal, orgID uuid.UUID, tokenHash []byte) (uuid.UUID, error) {
	if len(tokenHash) > 0 {
		var id, invitationOrg uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id,org_id FROM invitations WHERE token_hash=$1`, tokenHash).Scan(&id, &invitationOrg)
		if isNoRows(err) || (err == nil && invitationOrg != orgID) {
			return uuid.Nil(), ErrInvalidToken
		}
		if err != nil {
			return uuid.Nil(), err
		}
		return acceptInvitationLocked(ctx, tx, cfg, current, id, orgID)
	}
	var member uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL AND suspended_at IS NULL FOR SHARE`, orgID, current.UserID).Scan(&member)
	if isNoRows(err) {
		return uuid.Nil(), ErrSSONotEligible
	}
	return member, err
}

// bindSSO links the identity to the member and gives the current browser
// session the resulting proof.
func bindSSO(ctx context.Context, tx pgx.Tx, cfg Config, p auth.Principal, orgID, member uuid.UUID, a SSOAssertion) error {
	b, created, err := sso.Link(ctx, tx, orgID, a.ConnectionID, a.Subject, member, p.UserID)
	if errors.Is(err, sso.ErrIdentityConflict) {
		return ErrSSOIdentityConflict
	}
	if err != nil {
		return err
	}
	if err := admitSSOProof(ctx, tx, b, a.AuthenticatedAt); err != nil {
		return err
	}
	if created {
		evidence := audit.FromPrincipal(p)
		if err := audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: evidence, OrgID: orgID, Action: "sso.binding_created", TargetKind: "member", TargetID: member,
			Details: audit.SSOBindingDetails{ConnectionID: a.ConnectionID, IdentityID: b.IdentityID, Subject: a.Subject}}); err != nil {
			return err
		}
	}
	return addSSOProof(ctx, tx, cfg, p.SessionID, b, a)
}

// admitSSOProof refuses an IdP authentication that would not admit the member:
// one already past the organization's proof lifetime, or one not after the
// member's sign-in cutoff. Both need a fresh IdP sign-in.
func admitSSOProof(ctx context.Context, tx pgx.Tx, b sso.Binding, at time.Time) error {
	var cutoff pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT reauthenticate_after FROM org_members WHERE id=$1 AND org_id=$2 AND user_id=$3 AND removed_at IS NULL AND suspended_at IS NULL FOR SHARE`, b.MemberID, b.OrgID, b.UserID).Scan(&cutoff)
	if isNoRows(err) {
		return ErrSSONotEligible
	}
	if err != nil {
		return err
	}
	if cutoff.Valid && !at.After(cutoff.Time) {
		return ErrSSOReauthenticationRequired
	}
	return freshSSOTime(ctx, tx, b.OrgID, at)
}

// freshSSOTime refuses an IdP authentication already past the organization's
// proof lifetime.
func freshSSOTime(ctx context.Context, tx pgx.Tx, org uuid.UUID, at time.Time) error {
	lifetime, err := sso.ProofLifetime(ctx, tx, org)
	if err != nil {
		return err
	}
	if !at.Add(lifetime).After(time.Now()) {
		return ErrSSOReauthenticationRequired
	}
	return nil
}

func addSSOProof(ctx context.Context, tx pgx.Tx, cfg Config, session uuid.UUID, b sso.Binding, a SSOAssertion) error {
	if err := sso.AddProof(ctx, tx, session, b, a.AuthenticatedAt); err != nil {
		return err
	}
	actor := audit.Evidence{Actor: audit.Actor{UserID: b.UserID}, Authentication: audit.Authentication{Origin: "request", Result: "verified", CredentialKind: "sso_assertion", CredentialID: b.IdentityID}, Authorization: audit.NotEvaluated()}
	return appendSessionLifecycle(ctx, tx, cfg, actor, pgvalue.UUID(b.OrgID), "identity.sign_in", session, audit.AuthenticationDetails{Method: "sso", Provider: a.Protocol, Subject: a.Subject})
}

// issueSSOSession starts a browser session with no ordinary source. It can
// enter only organizations for which it holds a live SSO proof; an ordinary
// sign-in by the same account later confirms it in place.
func issueSSOSession(ctx context.Context, tx pgx.Tx, cfg Config, user uuid.UUID) (string, uuid.UUID, error) {
	raw, err := auth.GenerateOpaque(loginSessionTokenBytes)
	if err != nil {
		return "", uuid.Nil(), err
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", uuid.Nil(), err
	}
	id := uuid.NewV7()
	if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, user); err != nil {
		return "", uuid.Nil(), err
	}
	_, err = tx.Exec(ctx, `INSERT INTO login_sessions(id,user_id,channel,token_hash,expires_at) VALUES($1,$2,'browser',$3,clock_timestamp()+interval '30 days')`, id, user, hash)
	return raw, id, err
}

// AccountSSOBindings lists the account's live company SSO links.
func AccountSSOBindings(ctx context.Context, q db.DBTX, p auth.Principal) ([]sso.AccountBinding, error) {
	return sso.AccountBindings(ctx, q, p.UserID)
}
