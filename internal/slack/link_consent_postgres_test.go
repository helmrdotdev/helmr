package slack

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"testing"
	"uuid"
)

func linkTestUser(ctx context.Context, pool db.TxBeginner, policy auditpolicy.Policy, actor audit.Evidence, org, user, installation uuid.UUID, identity UserIdentity) error {
	binding := make([]byte, 32)
	id, err := PrepareUserLink(ctx, pool, actor, org, user, installation, identity, binding)
	if err != nil {
		return err
	}
	return LinkUser(ctx, pool, policy, actor, org, user, installation, identity, LinkConsent{ID: id, BrowserHash: binding})
}

func TestSlackConsentIsOneUseAndLinkHasIndependentLifetime(t *testing.T) {
	f, _, org := credentialFixture(t)
	p := f.Principal(t)
	actor := audit.FromPrincipal(p)
	who := UserIdentity{TeamID: "team", SlackUserID: "consenting-human"}
	binding := bytes.Repeat([]byte{3}, 32)
	id, err := PrepareUserLink(t.Context(), f.Pool, actor, org, f.User, f.installation, who, binding)
	if err != nil {
		t.Fatal(err)
	}
	consent := LinkConsent{ID: id, BrowserHash: binding}
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	wrong := consent
	wrong.BrowserHash = bytes.Repeat([]byte{4}, 32)
	if err = LinkUser(t.Context(), f.Pool, policy, actor, org, f.User, f.installation, who, wrong); !errors.Is(err, ErrUserLinkDenied) {
		t.Fatal("other browser consumed consent", err)
	}
	if err = LinkUser(t.Context(), f.Pool, policy, actor, org, f.User, f.installation, who, consent); err != nil {
		t.Fatal(err)
	}
	if err = LinkUser(t.Context(), f.Pool, policy, actor, org, f.User, f.installation, who, consent); !errors.Is(err, ErrUserLinkDenied) {
		t.Fatal("replayed consent", err)
	}
	var link uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_user_links WHERE user_id=$1 AND revoked_at IS NULL`, f.User).Scan(&link); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, p.SessionID)
	if _, err = access.SlackPrincipal(t.Context(), f.Pool, link); err != nil {
		t.Fatal("browser logout revoked independent link", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, p.IdentityID)
	if _, err = access.SlackPrincipal(t.Context(), f.Pool, link); err != nil {
		t.Fatal("source retirement revoked independent link", err)
	}
	fresh := audit.FromPrincipal(f.Principal(t))
	next, err := PrepareUserLink(t.Context(), f.Pool, fresh, org, f.User, f.installation, who, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err = LinkUser(t.Context(), f.Pool, policy, fresh, org, f.User, f.installation, who, LinkConsent{ID: next, BrowserHash: binding}); err != nil {
		t.Fatal("existing independent link denied", err)
	}
	if _, err = access.SlackPrincipal(t.Context(), f.Pool, link); err != nil {
		t.Fatal("existing independent link lost", err)
	}

}

func TestSlackConsentSurvivesSigningInAgainInTheSameBrowser(t *testing.T) {
	f, _, org := credentialFixture(t)
	p := f.Principal(t)
	actor := audit.FromPrincipal(p)
	who := UserIdentity{TeamID: "team", SlackUserID: "human"}
	binding := make([]byte, 32)
	id, err := PrepareUserLink(t.Context(), f.Pool, actor, org, f.User, f.installation, who, binding)
	if err != nil {
		t.Fatal(err)
	}
	replacement := f.Principal(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET identity_id=$2,authenticated_at=clock_timestamp() WHERE id=$1`, p.SessionID, replacement.IdentityID)
	if err = LinkUser(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, org, f.User, f.installation, who, LinkConsent{ID: id, BrowserHash: binding}); err != nil {
		t.Fatal("consent in the same browser session was refused", err)
	}
}

func TestSlackConsentNeedsTheBrowserSessionThatSawIt(t *testing.T) {
	f, _, org := credentialFixture(t)
	p := f.Principal(t)
	who := UserIdentity{TeamID: "team", SlackUserID: "human"}
	binding := make([]byte, 32)
	id, err := PrepareUserLink(t.Context(), f.Pool, audit.FromPrincipal(p), org, f.User, f.installation, who, binding)
	if err != nil {
		t.Fatal(err)
	}
	other := audit.FromPrincipal(f.Principal(t))
	if err = LinkUser(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, other, org, f.User, f.installation, who, LinkConsent{ID: id, BrowserHash: binding}); !errors.Is(err, ErrUserLinkDenied) {
		t.Fatal("another browser session completed consent", err)
	}
}

func TestRetiredSlackAuthorizationCanRelinkWithoutRevivingOldWork(t *testing.T) {
	for _, scope := range []string{"account", "member"} {
		t.Run(scope, func(t *testing.T) {
			f, _, org := credentialFixture(t)
			p := f.Principal(t)
			actor := audit.FromPrincipal(p)
			who := UserIdentity{TeamID: "team", SlackUserID: "human"}
			policy := auditpolicy.Policy{RetentionSeconds: 3600}
			if err := linkTestUser(t.Context(), f.Pool, policy, actor, org, f.User, f.installation, who); err != nil {
				t.Fatal(err)
			}
			var old uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_user_links WHERE user_id=$1 AND revoked_at IS NULL`, f.User).Scan(&old); err != nil {
				t.Fatal(err)
			}
			pending, err := PrepareUserLink(t.Context(), f.Pool, actor, org, f.User, f.installation, who, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			err = db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				switch scope {
				case "account":
					return RetireAccountAccess(t.Context(), tx, f.User)
				default:
					return RetireMemberAccess(t.Context(), tx, org, p.MemberID)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = LinkUser(t.Context(), f.Pool, policy, actor, org, f.User, f.installation, who, LinkConsent{ID: pending, BrowserHash: make([]byte, 32)}); !errors.Is(err, ErrUserLinkDenied) {
				t.Fatal("pending consent survived", err)
			}
			if err = linkTestUser(t.Context(), f.Pool, policy, actor, org, f.User, f.installation, who); err != nil {
				t.Fatal("fresh relink", err)
			}
			if _, err = access.SlackPrincipal(t.Context(), f.Pool, old); !errors.Is(err, access.ErrDenied) {
				t.Fatal("unexpected old link lifetime", err)
			}
			var fresh uuid.UUID
			if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_user_links WHERE user_id=$1 AND revoked_at IS NULL`, f.User).Scan(&fresh); err != nil || fresh == old {
				t.Fatal(fresh, err)
			}
			if _, err = access.SlackPrincipal(t.Context(), f.Pool, fresh); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSlackPendingConsentRetainsBrowserSessionAndMembership(t *testing.T) {
	for _, change := range []string{"remove-reinvite", "browser-logout", "epoch"} {
		t.Run(change, func(t *testing.T) {
			f, _, org := credentialFixture(t)
			p := f.Principal(t)
			actor := audit.FromPrincipal(p)
			who := UserIdentity{TeamID: "team", SlackUserID: "pending-human"}
			binding := make([]byte, 32)
			id, err := PrepareUserLink(t.Context(), f.Pool, actor, org, f.User, f.installation, who, binding)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "remove-reinvite":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE id=$1; INSERT INTO org_members(id,org_id,user_id) VALUES($2,$3,$4)`, pgx.QueryExecModeSimpleProtocol, p.MemberID, uuid.NewV7(), org, f.User)
			case "browser-logout":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, p.SessionID)
			case "epoch":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE users SET access_epoch=access_epoch+1 WHERE id=$1`, f.User)
			}
			if err = LinkUser(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, org, f.User, f.installation, who, LinkConsent{ID: id, BrowserHash: binding}); !errors.Is(err, ErrUserLinkDenied) {
				t.Fatal("stale consent accepted", err)
			}
		})
	}
}
