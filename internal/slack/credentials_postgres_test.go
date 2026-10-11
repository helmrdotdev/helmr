package slack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func credentialFixture(t *testing.T) (statusFixture, *CredentialStore, uuid.UUID) {
	t.Helper()
	f := newStatusFixture(t)
	var org uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT organization_id FROM slack_installations WHERE id=$1`, f.installation).Scan(&org); err != nil {
		t.Fatal(err)
	}
	store, err := NewCredentialStore(f.Pool, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	registration := f.registration
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE org_id=$1 AND user_id=$2`, org, f.User)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_app_registrations SET credential_revision=0,client_id=NULL,credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, registration)
	if err = store.StoreAppCredentials(t.Context(), org, f.User, registration, AppCredentials{ClientID: "client", ClientSecret: "secret", SigningSecret: "signing-secret"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET app_registration_id=$2 WHERE id=$1`, f.installation, registration)
	expires := time.Now().Add(time.Minute)
	ciphertext, nonce, err := store.seal(org, f.installation, 1, credentialBundle{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET credential_ciphertext=$2,credential_nonce=$3,credential_expires_at=$4 WHERE id=$1`, f.installation, ciphertext, nonce, expires)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE org_id=$1 AND user_id=$2`, org, f.User)
	return f, store, org
}

func refreshResult() oauthResult {
	expires := time.Now().Add(12 * time.Hour)
	return oauthResult{grant: oauthGrant{bundle: credentialBundle{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: &expires}, app: "app", team: "team", bot: "bot", scopes: agent.RequiredSlackScopes()}}
}

func TestCredentialRefreshSerializesAndPreservesPublicationAuthority(t *testing.T) {
	f, store, _ := credentialFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	postClaim := f.postClaim(t, post)
	claim, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || claim == nil {
		t.Fatal("refresh claim failed", err)
	}
	next, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || next != nil {
		t.Fatal("duplicate refresh admitted", err)
	}
	if token, err := store.BotToken(t.Context(), f.installation, 1); err == nil || token != "" {
		t.Fatal("in-flight refresh exposed old token")
	}
	if err := FinishPost(t.Context(), f.Pool, postClaim, DeliveryResult{Disposition: Rejected, Code: "token_expired"}); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT p.status='pending' AND p.inflight_payload=$2 AND i.authorization_lost_at IS NULL FROM slack_posts p CROSS JOIN slack_installations i WHERE p.id=$1 AND i.id=$3`, post, postClaim.Payload, f.installation).Scan(&preserved); err != nil || !preserved {
		t.Fatal("refresh lost pending content", err)
	}
	if err := store.finishRefresh(t.Context(), *claim, refreshResult()); err != nil {
		t.Fatal(err)
	}
	// Repeat after a simulated lost commit acknowledgment: recognize the same
	// committed attempt, without a new revision or another token exchange.
	if err := store.finishRefresh(t.Context(), *claim, refreshResult()); err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT credential_revision=2 AND authorized_at=$2 AND last_refresh_attempt_id=$3 FROM slack_installations WHERE id=$1`, f.installation, claim.authorized, claim.attempt).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("refresh changed authorization or repeated commit", err)
	}
	if token, err := store.BotToken(t.Context(), f.installation, 2); err != nil || token != "new-access" {
		t.Fatal("replacement unavailable", err)
	}
	if token, err := store.BotToken(t.Context(), f.installation, 1); err == nil || token != "" {
		t.Fatal("old revision exposed")
	}
	f.due(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET next_attempt_at=clock_timestamp() WHERE id=$1`, post)
	retry := f.postClaim(t, post)
	if retry.CredentialRevision != 2 || !bytes.Equal(retry.Payload, postClaim.Payload) {
		t.Fatal("retry changed frozen payload or credential")
	}
	if err := FinishPost(t.Context(), f.Pool, retry, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialRefreshLossSuppressesOnlyUnsentAndRejectsLateReplacement(t *testing.T) {
	f, store, _ := credentialFixture(t)
	uncertain := f.post(t, 1, "uncertain", "lifecycle", nil)
	c := f.postClaim(t, uncertain)
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	pending := f.post(t, 2, "pending", "lifecycle", nil)
	claim, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET refresh_deadline=clock_timestamp()-interval '1 second' WHERE id=$1`, f.installation)
	if err := store.finishRefresh(t.Context(), *claim, refreshResult()); err != nil {
		t.Fatal(err)
	}
	var lost bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT credential_revision=1 AND authorization_lost_at IS NOT NULL AND refresh_error='refresh_replacement_unrecoverable' AND credential_ciphertext IS NOT NULL AND refresh_attempt_id IS NULL FROM slack_installations WHERE id=$1`, f.installation).Scan(&lost); err != nil || !lost {
		t.Fatal("late credential replaced custody or loss was not recorded", err)
	}
	for _, tc := range []struct {
		id    uuid.UUID
		state string
	}{{uncertain, "uncertain"}, {pending, "suppressed"}} {
		var state string
		if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM slack_posts WHERE id=$1`, tc.id).Scan(&state); err != nil || state != tc.state {
			t.Fatal("incorrect suppression", state, err)
		}
	}
	if token, err := store.BotToken(t.Context(), f.installation, 1); err == nil || token != "" {
		t.Fatal("unrecoverable exchange exposed old token")
	}
}

func (f statusFixture) reauthorize(t *testing.T, store *CredentialStore, org uuid.UUID, grant oauthGrant) (uuid.UUID, error) {
	t.Helper()
	observed, err := store.publicationAuthorization(t.Context(), org, f.User, f.publication)
	if err != nil {
		return uuid.Nil(), err
	}
	return store.attachPublicationGrant(t.Context(), org, f.User, f.publication, observed, grant)
}

func TestCredentialReauthorizationFencesRefreshAndDisconnectRetiresGeneration(t *testing.T) {
	f, store, org := credentialFixture(t)
	claim, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	grant := refreshResult().grant
	grant.bundle = credentialBundle{AccessToken: "reauthorized"}
	same, err := f.reauthorize(t, store, org, grant)
	if err != nil || same != f.installation {
		t.Fatal("reauthorization changed generation", err)
	}
	if err = store.finishRefresh(t.Context(), *claim, refreshResult()); err != nil {
		t.Fatal(err)
	}
	if token, err := store.BotToken(t.Context(), f.installation, 2); err != nil || token != "reauthorized" {
		t.Fatal("late refresh overwrote reauthorization", err)
	}
	if err = store.DisconnectPublication(t.Context(), org, f.User, f.Environment, f.publication); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT i.disconnected_at IS NOT NULL AND i.credential_ciphertext IS NULL AND i.credential_nonce IS NULL AND p.revoked_at IS NOT NULL AND r.retired_at IS NOT NULL AND r.credential_ciphertext IS NULL FROM slack_installations i JOIN agent_publications p ON p.slack_installation_id=i.id JOIN slack_app_registrations r ON r.id=i.app_registration_id WHERE i.id=$1`, f.installation).Scan(&retained); err != nil || !retained {
		t.Fatal("retirement retained secret material", retained, err)
	}
	if token, err := store.BotToken(t.Context(), f.installation, 2); err == nil || token != "" {
		t.Fatal("retired credential usable", err)
	}
	if _, err = f.reauthorize(t, store, org, grant); !errors.Is(err, ErrPublicationUnavailable) {
		t.Fatal("retired generation resurrected", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	setup, err := BeginPublication(t.Context(), f.Pool, org, f.User, f.Environment, f.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StoreAppCredentials(t.Context(), org, f.User, setup.RegistrationID, AppCredentials{ClientID: "next-client", ClientSecret: "next-secret", SigningSecret: "next-signing"}); err != nil {
		t.Fatal(err)
	}
	f.publication = setup.ID
	grant.app = "next-app"
	next, err := f.reauthorize(t, store, org, grant)
	if err != nil || next == f.installation {
		t.Fatal("reconnect did not create generation", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='developer' WHERE org_id=$1 AND user_id=$2`, org, f.User)
	if _, err = f.reauthorize(t, store, org, grant); !errors.Is(err, errInstallationAuthority) {
		t.Fatal("non-manager reauthorized", err)
	}
	if err = store.DisconnectPublication(t.Context(), org, f.User, f.Environment, f.publication); !errors.Is(err, errInstallationAuthority) {
		t.Fatal("non-manager disconnected", err)
	}
}

func TestCredentialRefreshHTTPRunsOutsideTransactionAndDoesNotRepeatUnknownExchange(t *testing.T) {
	f, store, _ := credentialFixture(t)
	calls := 0
	oauth, err := NewOAuthClient("client", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		tx, err := f.Pool.Begin(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(r.Context())
		if _, err := tx.Exec(r.Context(), `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, f.installation); err != nil {
			t.Fatal("database lock spans HTTP", err)
		}
		return nil, errors.New("unknown transport outcome")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := store.Refresh(t.Context(), f.installation, oauth); err != nil || !worked {
		t.Fatal("exchange failed to resolve", err)
	}
	if worked, err := store.Refresh(t.Context(), f.installation, oauth); err != nil || worked || calls != 1 {
		t.Fatal("unknown exchange repeated", calls, err)
	}
}

func TestCredentialRefreshRateLimitRetainsCustodyUntilRetry(t *testing.T) {
	f, store, _ := credentialFixture(t)
	oauth, err := NewOAuthClient("client", "secret", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"7"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := store.Refresh(t.Context(), f.installation, oauth); err != nil || !worked {
		t.Fatal(err)
	}
	var ready bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT authorization_lost_at IS NULL AND credential_revision=1 AND refresh_attempt_id IS NULL AND refresh_next_at>clock_timestamp() FROM slack_installations WHERE id=$1`, f.installation).Scan(&ready); err != nil || !ready {
		t.Fatal("rate limit lost custody or retry boundary", err)
	}
	if token, err := store.BotToken(t.Context(), f.installation, 1); err != nil || token != "old-access" {
		t.Fatal("rate limit made valid token unavailable", err)
	}
}

// Drop just the commit acknowledgment after PostgreSQL commits, proving that the
// credential owner recovers an actual durable replacement rather than exchanging
// again. The transaction protocol is otherwise the real database implementation.
type lostRefreshCommit struct {
	db.TxBeginner
	lose bool
}
type lostRefreshTx struct {
	pgx.Tx
	owner *lostRefreshCommit
}

func (p *lostRefreshCommit) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &lostRefreshTx{Tx: tx, owner: p}, nil
}
func (tx *lostRefreshTx) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	if err == nil && tx.owner.lose {
		tx.owner.lose = false
		return io.ErrUnexpectedEOF
	}
	return err
}

func TestCredentialRefreshRecoversLostDatabaseAcknowledgment(t *testing.T) {
	f, store, _ := credentialFixture(t)
	claim, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	failing := &lostRefreshCommit{TxBeginner: f.Pool, lose: true}
	store.pool = failing
	if err := store.finishRefresh(t.Context(), *claim, refreshResult()); err == nil {
		t.Fatal("commit acknowledgment was not lost")
	}
	if err := store.finishRefresh(t.Context(), *claim, refreshResult()); err != nil {
		t.Fatal("durable replacement not recovered", err)
	}
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT credential_revision FROM slack_installations WHERE id=$1`, f.installation).Scan(&revision); err != nil || revision != 2 {
		t.Fatal("replacement repeated", revision, err)
	}
}

func TestCredentialChangeBeforeHTTPKeepsExactUnsentMutation(t *testing.T) {
	f, store, _ := credentialFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	refresh, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || refresh == nil {
		t.Fatal(err)
	}
	if err := store.finishRefresh(t.Context(), *refresh, refreshResult()); err != nil {
		t.Fatal(err)
	}
	if allowed, err := postClaimAuthorized(t.Context(), f.Pool, claim); err != nil || !allowed {
		t.Fatal("routine refresh removed publication authority", err)
	}
	calls := 0
	client := NewWebClient(store, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not issue stale credentials")
	}))
	result := client.Call(t.Context(), claim.InstallationID, claim.CredentialRevision, claim.Method, claim.Payload)
	if result.Disposition != NotIssued || calls != 0 {
		t.Fatal("stale credential request was issued")
	}
	if err := FinishPost(t.Context(), f.Pool, claim, result); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND inflight_payload=$2 AND inflight_attempt_id=$3 FROM slack_posts WHERE id=$1`, post, claim.Payload, claim.AttemptID).Scan(&preserved); err != nil || !preserved {
		t.Fatal("proven-unsent request identity lost", err)
	}
}

func TestCredentialReauthorizationDoesNotBackfillKnownUnsentCompletion(t *testing.T) {
	for _, disposition := range []Disposition{NotIssued, RateLimited, Rejected} {
		t.Run(string(disposition), func(t *testing.T) {
			f, store, org := credentialFixture(t)
			post := f.post(t, 1, "content", "lifecycle", nil)
			claim := f.postClaim(t, post)
			if _, err := f.reauthorize(t, store, org, refreshResult().grant); err != nil {
				t.Fatal(err)
			}
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: disposition, Code: "token_expired", RetryAfter: time.Second}); err != nil {
				t.Fatal(err)
			}
			var suppressed bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='suppressed' AND inflight_payload IS NULL FROM slack_posts WHERE id=$1`, post).Scan(&suppressed); err != nil || !suppressed {
				t.Fatal("old content entered new authorization window", err)
			}
		})
	}
}

func TestCredentialExpiredOwnerPausesWithoutRepeatingExchange(t *testing.T) {
	f, store, _ := credentialFixture(t)
	claim, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET refresh_deadline=clock_timestamp()-interval '1 second' WHERE id=$1`, f.installation)
	recovered, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || recovered != nil {
		t.Fatal("expired owner admitted exchange", err)
	}
	var lost bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT authorization_lost_at IS NOT NULL AND refresh_error='refresh_replacement_unrecoverable' AND credential_revision=1 FROM slack_installations WHERE id=$1`, f.installation).Scan(&lost); err != nil || !lost {
		t.Fatal("expired attempt not resolved", err)
	}
}

func TestCredentialRefreshRecoversClaimCommitBeforeIssuingHTTP(t *testing.T) {
	f, store, _ := credentialFixture(t)
	store.pool = &lostRefreshCommit{TxBeginner: f.Pool, lose: true}
	claim, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || claim == nil {
		t.Fatal("committed claim not recovered", err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT refresh_attempt_id=$2 FROM slack_installations WHERE id=$1`, f.installation, claim.attempt).Scan(&exact); err != nil || !exact {
		t.Fatal("recovered uncommitted claim", err)
	}
}

func TestCredentialReauthorizationSuppressesDesiredUpdateAfterOldConfirmation(t *testing.T) {
	for _, path := range []string{"acknowledgment", "reconciliation"} {
		t.Run(path, func(t *testing.T) {
			f, store, org := credentialFixture(t)
			post := f.post(t, 1, "content", "lifecycle", nil)
			claim := f.postClaim(t, post)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET desired_revision=2 WHERE id=$1`, post)
			if path == "reconciliation" {
				if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.reauthorize(t, store, org, refreshResult().grant); err != nil {
				t.Fatal(err)
			}
			if path == "reconciliation" {
				if confirmed, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, claim)); err != nil || !confirmed {
					t.Fatal("issued content not reconciled", err)
				}
			} else if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
				t.Fatal(err)
			}
			var exact bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='suppressed' AND confirmed_revision=1 AND desired_revision=2 AND suppressed_revision=2 AND message_ts IS NOT NULL AND inflight_payload IS NULL FROM slack_posts WHERE id=$1`, post).Scan(&exact); err != nil || !exact {
				t.Fatal("confirmation backfilled an old desired revision", err)
			}
			f.due(t)
			f.noPostClaim(t, post)
		})
	}
}

func TestCredentialRefreshPersistsReplacementAfterCallerCancellation(t *testing.T) {
	f, store, _ := credentialFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	oauth, err := NewOAuthClient("client", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"token_type":"bot","access_token":"new-access","refresh_token":"new-refresh","expires_in":43200}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := store.Refresh(ctx, f.installation, oauth); err != nil || !worked || calls != 1 {
		t.Fatal("cancelled caller lost a recoverable exchange", calls, err)
	}
	if token, err := store.BotToken(t.Context(), f.installation, 2); err != nil || token != "new-access" {
		t.Fatal("replacement was not persisted", err)
	}
}
