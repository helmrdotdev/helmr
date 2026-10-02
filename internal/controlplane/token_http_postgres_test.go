package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// tokenHTTPFixture serves NewServer over a run fixture's environment with an
// API key that holds every Token permission.
type tokenHTTPFixture struct {
	run     runtest.Fixture
	handler http.Handler
	key     string
}

// withTokenPublicURL serves Token callback URLs from the console origin.
func withTokenPublicURL(cfg *ServerConfig) {
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
}

func newTokenHTTPFixture(t *testing.T) tokenHTTPFixture {
	t.Helper()
	fixture := runtest.New(t)
	handler := newPostgresServer(t, fixture.Pool, withTokenPublicURL)
	key := issueEnvironmentAPIKey(t, fixture.Pool, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID,
		auth.PermissionTokensCreate, auth.PermissionTokensRead, auth.PermissionTokensComplete, auth.PermissionTokensCancel)
	return tokenHTTPFixture{run: fixture, handler: handler, key: key}
}

// keyWith issues another environment API key holding only permissions.
func (f tokenHTTPFixture) keyWith(t *testing.T, permissions ...auth.Permission) string {
	t.Helper()
	return issueEnvironmentAPIKey(t, f.run.Pool, f.run.OrgID, f.run.ProjectID, f.run.EnvironmentID, permissions...)
}

// sessionToken returns a login session of an owner of the fixture
// organization.
func (f tokenHTTPFixture) sessionToken(t *testing.T) string {
	t.Helper()
	keys, err := auth.NewKeys(testAuthRootKey())
	if err != nil {
		t.Fatal(err)
	}
	sessions := httpPostgresFixture{pool: f.run.Pool, queries: db.New(f.run.Pool), handler: f.handler, keys: keys}
	userID := sessions.user(t, "Token Owner")
	sessions.member(t, f.run.OrgID, userID, db.OrgMemberRoleOwner)
	return sessions.session(t, userID, f.run.OrgID)
}

func (f tokenHTTPFixture) environmentPath() string {
	return "/api/projects/" + f.run.ProjectID.String() + "/environments/" + f.run.EnvironmentID.String()
}

func (f tokenHTTPFixture) serve(t *testing.T, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAPIKey(f.handler, method, path, key, body)
}

// create creates a Token through the API key route and returns its create
// response.
func (f tokenHTTPFixture) create(t *testing.T, body string) api.TokenResponse {
	t.Helper()
	response := f.serve(t, http.MethodPost, "/v1/tokens", f.key, body)
	return decodeTokenStatus(t, response, http.StatusCreated)
}

// expire moves a pending Token's expiry into the past.
func (f tokenHTTPFixture) expire(t *testing.T, tokenID string) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.run.Pool, `UPDATE tokens SET created_at=now()-interval '2 minutes', expires_at=now()-interval '1 minute' WHERE id=$1`, tokenID)
}

// failControlOutbox makes every later control outbox insert fail, so a Token
// transition that publishes its reconciliation intent fails internally.
func (f tokenHTTPFixture) failControlOutbox(t *testing.T) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.run.Pool, `CREATE FUNCTION fail_token_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'control outbox is unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.run.Pool, `CREATE TRIGGER fail_token_outbox BEFORE INSERT ON control_outbox FOR EACH ROW EXECUTE FUNCTION fail_token_outbox()`)
}

func (f tokenHTTPFixture) tokenStatus(t *testing.T, tokenID string) string {
	t.Helper()
	var status string
	if err := f.run.Pool.QueryRow(t.Context(), `SELECT status FROM tokens WHERE id=$1`, tokenID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func decodeTokenStatus(t *testing.T, response *httptest.ResponseRecorder, want int) api.TokenResponse {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
	var token api.TokenResponse
	if err := json.Unmarshal(response.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	return token
}

// assertTokenHTTPError requires the status and the error code and message.
func assertTokenHTTPError(t *testing.T, response *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	body := decodeHTTPError(t, response.Body.Bytes())
	if body.Code != code || body.Message != message {
		t.Fatalf("error = %+v, want %s %q", body, code, message)
	}
}

func callbackPath(t *testing.T, token api.TokenResponse) string {
	t.Helper()
	parsed, err := url.Parse(token.CallbackURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Path
}

func (f tokenHTTPFixture) bearerComplete(t *testing.T, tokenID, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAPIKey(f.handler, http.MethodPost, "/api/public/tokens/"+tokenID+"/complete", bearer, body)
}

func TestTokenExternalCreateRoutes(t *testing.T) {
	f := newTokenHTTPFixture(t)
	body := `{"timeout":"1h","tags":["approval"],"metadata":{"review":true},"idempotency_key":"create-1"}`
	created := f.create(t, body)
	if created.Status != api.TokenStatusPending || created.PublicAccessToken == "" ||
		!strings.HasPrefix(created.CallbackURL, "https://console.example.test/api/token-callbacks/"+created.ID+"/") ||
		len(created.Tags) != 1 || created.Tags[0] != "approval" || string(created.Metadata) != `{"review":true}` ||
		!created.TimeoutAt.Equal(created.CreatedAt.Add(time.Hour)) {
		t.Fatalf("created = %+v", created)
	}
	var createdBy string
	if err := f.run.Pool.QueryRow(t.Context(), `SELECT created_by::text FROM public_access_tokens WHERE token_id=$1`, created.ID).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != `{"kind": "external"}` {
		t.Fatalf("created_by = %s", createdBy)
	}
	var receipt string
	if err := f.run.Pool.QueryRow(t.Context(), `SELECT receipt::text FROM idempotency_claims WHERE environment_id=$1 AND status='completed'`, f.run.EnvironmentID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt != `{"token_id": "`+created.ID+`"}` {
		t.Fatalf("receipt = %s", receipt)
	}

	replayed := decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens", f.key, body), http.StatusOK)
	if replayed.ID != created.ID || replayed.PublicAccessToken != created.PublicAccessToken || replayed.CallbackURL != created.CallbackURL {
		t.Fatalf("replay = %+v, want %+v", replayed, created)
	}
	// The replay projects the Token as created even after it completed.
	decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+created.ID+"/complete", f.key, `{"result":{"approved":true}}`), http.StatusOK)
	replayed = decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens", f.key, body), http.StatusOK)
	if replayed.ID != created.ID || replayed.Status != api.TokenStatusPending || replayed.Result != nil {
		t.Fatalf("completed replay = %+v", replayed)
	}

	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens", f.key, `{"timeout":"2h","idempotency_key":"create-1"}`),
		http.StatusConflict, "idempotency_conflict", "idempotency key conflicts with an earlier token operation")
	if first, second := f.create(t, `{}`), f.create(t, `{}`); first.ID == second.ID {
		t.Fatal("creates without an idempotency key converged")
	}

	dbtest.MustExec(t, t.Context(), f.run.Pool, `UPDATE idempotency_claims SET receipt=NULL, receipt_expires_at=now()-interval '1 minute', receipt_pruned_at=now() WHERE environment_id=$1`, f.run.EnvironmentID)
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens", f.key, body),
		http.StatusGone, "operation_expired", "operation receipt has expired")

	for name, test := range map[string]struct {
		body    string
		message string
	}{
		"timeout":     {`{"timeout":"later"}`, ""},
		"annotations": {`{"metadata":[]}`, ""},
		"key":         {`{"idempotency_key":" padded"}`, "idempotency_key cannot begin or end with whitespace"},
	} {
		t.Run(name, func(t *testing.T) {
			response := f.serve(t, http.MethodPost, "/v1/tokens", f.key, test.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.message) {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
		})
	}
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens", f.keyWith(t, auth.PermissionTokensRead), `{}`),
		http.StatusForbidden, "forbidden", "permission is required")
	if response := f.serve(t, http.MethodPost, "/v1/tokens", "", `{}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create status = %d: %s", response.Code, response.Body.String())
	}

	session := f.sessionToken(t)
	sessionBody := `{"timeout":"5m","idempotency_key":"session-create"}`
	path := f.environmentPath() + "/tokens"
	sessionCreated := decodeTokenStatus(t, f.serve(t, http.MethodPost, path, session, sessionBody), http.StatusCreated)
	sessionReplayed := decodeTokenStatus(t, f.serve(t, http.MethodPost, path, session, sessionBody), http.StatusOK)
	if sessionReplayed.ID != sessionCreated.ID || sessionReplayed.PublicAccessToken != sessionCreated.PublicAccessToken {
		t.Fatalf("session replay = %+v, want %+v", sessionReplayed, sessionCreated)
	}
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/api/projects/"+uuid.NewV7().String()+"/environments/"+f.run.EnvironmentID.String()+"/tokens", session, `{}`),
		http.StatusBadRequest, "bad_request", "project_id must reference an active project")
}

// A creation with no API origin to serve its callback URL fails internally
// and creates nothing.
func TestTokenCreateWithoutAPIOriginCreatesNothing(t *testing.T) {
	fixture := runtest.New(t)
	handler := newPostgresServer(t, fixture.Pool)
	key := issueEnvironmentAPIKey(t, fixture.Pool, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, auth.PermissionTokensCreate)
	assertTokenHTTPError(t, serveAPIKey(handler, http.MethodPost, "/v1/tokens", key, `{"idempotency_key":"no-origin"}`),
		http.StatusInternalServerError, "internal_error", "internal server error")
	var tokens, credentials, claims int
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tokens), (SELECT count(*) FROM public_access_tokens), (SELECT count(*) FROM idempotency_claims)`).Scan(&tokens, &credentials, &claims); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 || credentials != 0 || claims != 0 {
		t.Fatalf("failed creation left tokens=%d credentials=%d claims=%d", tokens, credentials, claims)
	}
}

// liveTokenSource makes the lease's Run a live source that entered its
// entrypoint.
func liveTokenSource(t *testing.T, f runtest.Fixture, work runtest.RunLease) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)
}

func TestTokenRuntimeCreateRoute(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	liveTokenSource(t, f, work)
	worker := newWorkerHTTPClient(t, newPostgresServer(t, f.Pool, withTokenPublicURL), f.Pool, f.WorkerID)
	lease := workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1}
	request := workerapi.CreateTokenRequest{Lease: lease, CorrelationID: uuid.NewV7().String(), Tags: []string{"runtime"}}

	var created api.TokenResponse
	if response := worker.post(t, "/worker/v1/run/tokens/create", request, http.StatusCreated, nil); json.Unmarshal(response.Body.Bytes(), &created) != nil {
		t.Fatal(response.Body.String())
	}
	if created.Status != api.TokenStatusPending || created.PublicAccessToken == "" || created.CallbackURL == "" ||
		!created.TimeoutAt.Equal(created.CreatedAt.Add(10*time.Minute)) {
		t.Fatalf("created = %+v", created)
	}
	var createdBy string
	if err := f.Pool.QueryRow(t.Context(), `SELECT created_by::text FROM public_access_tokens WHERE token_id=$1`, created.ID).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != `{"kind": "runtime"}` {
		t.Fatalf("created_by = %s", createdBy)
	}
	var replayed api.TokenResponse
	worker.post(t, "/worker/v1/run/tokens/create", request, http.StatusOK, &replayed)
	if replayed.ID != created.ID || replayed.PublicAccessToken != created.PublicAccessToken || replayed.CallbackURL != created.CallbackURL {
		t.Fatalf("replay = %+v, want %+v", replayed, created)
	}
	conflicting := request
	conflicting.Tags = []string{"other"}
	response := worker.post(t, "/worker/v1/run/tokens/create", conflicting, http.StatusConflict, nil)
	if body := decodeHTTPError(t, response.Body.Bytes()); body.Code != "idempotency_conflict" {
		t.Fatalf("conflict = %s", response.Body.String())
	}

	invalid := request
	invalid.CorrelationID = "not-a-uuid"
	worker.post(t, "/worker/v1/run/tokens/create", invalid, http.StatusBadRequest, nil)
	timeout := int64(0)
	invalid = request
	invalid.CorrelationID = uuid.NewV7().String()
	invalid.TimeoutMS = &timeout
	worker.post(t, "/worker/v1/run/tokens/create", invalid, http.StatusBadRequest, nil)

	stale := workerapi.CreateTokenRequest{Lease: workerapi.RunLeaseFence{ID: lease.ID, LeaseSequence: 2}, CorrelationID: uuid.NewV7().String()}
	response = worker.post(t, "/worker/v1/run/tokens/create", stale, http.StatusConflict, nil)
	if body := decodeHTTPError(t, response.Body.Bytes()); body.Code != "conflict" || body.Message != "token create source authority is stale" {
		t.Fatalf("stale lease = %s", response.Body.String())
	}
	// A lease that addresses a Run which is not a live source is stale too,
	// after the idempotency claim.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=NULL WHERE run_id=$1`, work.RunID)
	notLive := workerapi.CreateTokenRequest{Lease: lease, CorrelationID: uuid.NewV7().String()}
	response = worker.post(t, "/worker/v1/run/tokens/create", notLive, http.StatusConflict, nil)
	if body := decodeHTTPError(t, response.Body.Bytes()); body.Code != "conflict" || body.Message != "token create source authority is stale" {
		t.Fatalf("stale source = %s", response.Body.String())
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, f.WorkerID)
	response = worker.post(t, "/worker/v1/run/tokens/create", workerapi.CreateTokenRequest{Lease: lease, CorrelationID: uuid.NewV7().String()}, http.StatusUnauthorized, nil)
	if body := decodeHTTPError(t, response.Body.Bytes()); body.Message != "worker authentication is required" {
		t.Fatalf("stale claims = %s", response.Body.String())
	}
}

func TestTokenManagementCompleteAndCancelRoutes(t *testing.T) {
	f := newTokenHTTPFixture(t)

	token := f.create(t, `{}`)
	completed := decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+token.ID+"/complete", f.key, `{"result":{"b":2,"a":1},"idempotency_key":"complete-1"}`), http.StatusOK)
	if completed.Status != api.TokenStatusCompleted || string(completed.Result) != `{"a":1,"b":2}` || completed.CompletedAt == nil {
		t.Fatalf("completed = %+v", completed)
	}
	var receipt string
	if err := f.run.Pool.QueryRow(t.Context(), `SELECT receipt::text FROM idempotency_claims WHERE environment_id=$1 AND status='completed'`, f.run.EnvironmentID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt != `{"outcome": "completed", "token_id": "`+token.ID+`"}` {
		t.Fatalf("receipt = %s", receipt)
	}
	var intents int
	if err := f.run.Pool.QueryRow(t.Context(), `SELECT count(*) FROM control_outbox WHERE topic='token.reconcile' AND payload->>'tokenId'=$1`, token.ID).Scan(&intents); err != nil || intents != 1 {
		t.Fatalf("reconcile intents = %d, %v", intents, err)
	}
	replayed := decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+token.ID+"/complete", f.key, `{"result":{"a":1,"b":2},"idempotency_key":"complete-1"}`), http.StatusOK)
	if replayed.ID != token.ID || replayed.Status != api.TokenStatusCompleted {
		t.Fatalf("replay = %+v", replayed)
	}
	decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+token.ID+"/complete", f.key, `{"result":{"a":1,"b":2}}`), http.StatusOK)
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+token.ID+"/complete", f.key, `{"result":{"a":2}}`),
		http.StatusConflict, "token_completion_conflict", "token completion conflicts with the existing result")
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+token.ID+"/complete", f.key, `{"result":{"a":2},"idempotency_key":"complete-1"}`),
		http.StatusConflict, "idempotency_conflict", "idempotency key conflicts with an earlier token operation")
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+token.ID+"/cancel", f.key, `{}`),
		http.StatusConflict, "token_completed", "token is already completed")

	cancelledToken := f.create(t, `{}`)
	cancelled := decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+cancelledToken.ID+"/cancel", f.key, `{"idempotency_key":"cancel-1"}`), http.StatusOK)
	if cancelled.Status != api.TokenStatusCancelled {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+cancelledToken.ID+"/cancel", f.key, `{"idempotency_key":"cancel-1"}`), http.StatusOK)
	decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+cancelledToken.ID+"/cancel", f.key, `{}`), http.StatusOK)
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+cancelledToken.ID+"/complete", f.key, `{"result":true}`),
		http.StatusConflict, "token_cancelled", "token was cancelled")

	for name, test := range map[string]struct{ path, body string }{
		"complete": {"/complete", `{"result":true,"idempotency_key":"expired-complete"}`},
		"cancel":   {"/cancel", `{"idempotency_key":"expired-cancel"}`},
	} {
		t.Run("expired "+name, func(t *testing.T) {
			expired := f.create(t, `{}`)
			f.expire(t, expired.ID)
			assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+expired.ID+test.path, f.key, test.body),
				http.StatusGone, "token_expired", "token has expired")
			// The expiry and the failed receipt are committed before the
			// operation reports it.
			if status := f.tokenStatus(t, expired.ID); status != "expired" {
				t.Fatalf("token status = %s", status)
			}
			var claimStatus, receipt string
			if err := f.run.Pool.QueryRow(t.Context(), `SELECT status, receipt::text FROM idempotency_claims WHERE environment_id=$1 AND receipt->>'token_id'=$2`, f.run.EnvironmentID, expired.ID).Scan(&claimStatus, &receipt); err != nil {
				t.Fatal(err)
			}
			if claimStatus != "failed" || receipt != `{"outcome": "expired", "token_id": "`+expired.ID+`"}` {
				t.Fatalf("claim = %s %s", claimStatus, receipt)
			}
			assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+expired.ID+test.path, f.key, test.body),
				http.StatusGone, "token_expired", "token has expired")
			// Without a key the expiry is reported as is.
			body := `{}`
			if name == "complete" {
				body = `{"result":true}`
			}
			assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+expired.ID+test.path, f.key, body),
				http.StatusGone, "token_expired", "token has expired")
		})
	}

	pending := f.create(t, `{}`)
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+pending.ID+"/complete", f.key, `{"result":{"a":1,"a":2}}`),
		http.StatusBadRequest, "bad_request", "result must be unambiguous JSON")
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+pending.ID+"/complete", f.key, `{}`),
		http.StatusBadRequest, "bad_request", "result is required")
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+uuid.NewV7().String()+"/complete", f.key, `{"result":true}`),
		http.StatusNotFound, "token_not_found", "token was not found")
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/not-a-token/cancel", f.key, `{}`),
		http.StatusNotFound, "token_not_found", "token was not found")
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+pending.ID+"/complete", f.keyWith(t, auth.PermissionTokensRead), `{"result":true}`),
		http.StatusForbidden, "forbidden", "permission is required")
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+pending.ID+"/cancel", f.keyWith(t, auth.PermissionTokensComplete), `{}`),
		http.StatusForbidden, "forbidden", "permission is required")

	session := f.sessionToken(t)
	completedBySession := decodeTokenStatus(t, f.serve(t, http.MethodPost, f.environmentPath()+"/tokens/"+pending.ID+"/complete", session, `{"result":"done"}`), http.StatusOK)
	if completedBySession.Status != api.TokenStatusCompleted {
		t.Fatalf("session completion = %+v", completedBySession)
	}
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, f.environmentPath()+"/tokens/"+pending.ID+"/cancel", session, `{}`),
		http.StatusConflict, "token_completed", "token is already completed")
	sessionCancelled := f.create(t, `{}`)
	cancelledBySession := decodeTokenStatus(t, f.serve(t, http.MethodPost, f.environmentPath()+"/tokens/"+sessionCancelled.ID+"/cancel", session, `{"idempotency_key":"session-cancel"}`), http.StatusOK)
	if cancelledBySession.ID != sessionCancelled.ID || cancelledBySession.Status != api.TokenStatusCancelled {
		t.Fatalf("session cancellation = %+v", cancelledBySession)
	}

	failing := f.create(t, `{}`)
	f.failControlOutbox(t)
	assertTokenHTTPError(t, f.serve(t, http.MethodPost, "/v1/tokens/"+failing.ID+"/complete", f.key, `{"result":true}`),
		http.StatusInternalServerError, "internal_error", "internal server error")
	if status := f.tokenStatus(t, failing.ID); status != "pending" {
		t.Fatalf("failed completion left token %s", status)
	}
}

func TestTokenPublicCompletionRoutes(t *testing.T) {
	f := newTokenHTTPFixture(t)
	scopeDenied := func(t *testing.T, response *httptest.ResponseRecorder) {
		t.Helper()
		assertTokenHTTPError(t, response, http.StatusUnauthorized, "token_scope_denied", "token credential is invalid")
	}

	t.Run("callback", func(t *testing.T) {
		token := f.create(t, `{}`)
		path := callbackPath(t, token)
		assertTokenHTTPError(t, f.serve(t, http.MethodPost, path, "", `{"result":{"a":1,"a":2}}`), http.StatusBadRequest, "bad_request", "result must be unambiguous JSON")
		scopeDenied(t, f.serve(t, http.MethodPost, path+"x", "", `{"result":true}`))
		scopeDenied(t, f.serve(t, http.MethodPost, "/api/token-callbacks/not-a-token/secret", "", `{"result":true}`))
		scopeDenied(t, f.serve(t, http.MethodPost, strings.Replace(path, token.ID, uuid.NewV7().String(), 1), "", `{"result":true}`))
		if response := f.serve(t, http.MethodPost, path, "", `{}`); response.Code != http.StatusBadRequest {
			t.Fatalf("missing result status = %d: %s", response.Code, response.Body.String())
		}
		completed := decodeTokenStatus(t, f.serve(t, http.MethodPost, path, "", `{"result":{"ok":true}}`), http.StatusOK)
		if completed.Status != api.TokenStatusCompleted || string(completed.Result) != `{"ok":true}` {
			t.Fatalf("completed = %+v", completed)
		}
		decodeTokenStatus(t, f.serve(t, http.MethodPost, path, "", `{"result":{"ok":true}}`), http.StatusOK)
		assertTokenHTTPError(t, f.serve(t, http.MethodPost, path, "", `{"result":{"ok":false}}`),
			http.StatusConflict, "token_completion_conflict", "token completion conflicts with the existing result")

		cancelled := f.create(t, `{}`)
		decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+cancelled.ID+"/cancel", f.key, `{}`), http.StatusOK)
		assertTokenHTTPError(t, f.serve(t, http.MethodPost, callbackPath(t, cancelled), "", `{"result":true}`),
			http.StatusConflict, "token_cancelled", "token was cancelled")

		expired := f.create(t, `{}`)
		f.expire(t, expired.ID)
		assertTokenHTTPError(t, f.serve(t, http.MethodPost, callbackPath(t, expired), "", `{"result":true}`),
			http.StatusGone, "token_expired", "token has expired")
		if status := f.tokenStatus(t, expired.ID); status != "expired" {
			t.Fatalf("expired token status = %s", status)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		token := f.create(t, `{}`)
		other := f.create(t, `{}`)
		scopeDenied(t, f.bearerComplete(t, token.ID, "", `{"result":true}`))
		denied := f.bearerComplete(t, token.ID, "hlmr_pub_wrong", `{"result":true}`)
		scopeDenied(t, denied)
		if denied.Header().Get("Access-Control-Allow-Origin") != "*" || denied.Header().Get("Vary") != "Origin" {
			t.Fatalf("denied bearer headers = %v", denied.Header())
		}
		scopeDenied(t, f.bearerComplete(t, token.ID, other.PublicAccessToken, `{"result":true}`))
		scopeDenied(t, f.bearerComplete(t, "not-a-token", token.PublicAccessToken, `{"result":true}`))
		assertTokenHTTPError(t, f.bearerComplete(t, token.ID, token.PublicAccessToken, `{"result":{"a":1,"a":2}}`), http.StatusBadRequest, "bad_request", "result must be unambiguous JSON")
		response := f.bearerComplete(t, token.ID, token.PublicAccessToken, `{"result":{"ok":true}}`)
		completed := decodeTokenStatus(t, response, http.StatusOK)
		if completed.Status != api.TokenStatusCompleted || response.Header().Get("Access-Control-Allow-Origin") != "*" || response.Header().Get("Vary") != "Origin" {
			t.Fatalf("completed = %+v headers=%v", completed, response.Header())
		}
		var used int
		if err := f.run.Pool.QueryRow(t.Context(), `SELECT used_count FROM public_access_tokens WHERE token_id=$1`, token.ID).Scan(&used); err != nil || used != 1 {
			t.Fatalf("used_count = %d, %v", used, err)
		}
		// Completing again with the same result enqueues nothing and does not
		// use the credential again.
		decodeTokenStatus(t, f.bearerComplete(t, token.ID, token.PublicAccessToken, `{"result":{"ok":true}}`), http.StatusOK)
		if err := f.run.Pool.QueryRow(t.Context(), `SELECT used_count FROM public_access_tokens WHERE token_id=$1`, token.ID).Scan(&used); err != nil || used != 1 {
			t.Fatalf("used_count after repeat = %d, %v", used, err)
		}
		assertTokenHTTPError(t, f.bearerComplete(t, token.ID, token.PublicAccessToken, `{"result":{"ok":false}}`),
			http.StatusConflict, "token_completion_conflict", "token completion conflicts with the existing result")

		cancelled := f.create(t, `{}`)
		decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+cancelled.ID+"/cancel", f.key, `{}`), http.StatusOK)
		assertTokenHTTPError(t, f.bearerComplete(t, cancelled.ID, cancelled.PublicAccessToken, `{"result":true}`),
			http.StatusConflict, "token_cancelled", "token was cancelled")

		// The Token's own expiry decides, while its credential is still live.
		expired := f.create(t, `{}`)
		f.expire(t, expired.ID)
		assertTokenHTTPError(t, f.bearerComplete(t, expired.ID, expired.PublicAccessToken, `{"result":true}`),
			http.StatusGone, "token_expired", "token has expired")

		request := httptest.NewRequest(http.MethodOptions, "/api/public/tokens/"+token.ID+"/complete", nil)
		preflight := httptest.NewRecorder()
		f.handler.ServeHTTP(preflight, request)
		if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Origin") != "*" ||
			preflight.Header().Get("Access-Control-Allow-Methods") != "POST, OPTIONS" ||
			preflight.Header().Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" {
			t.Fatalf("preflight = %d %v", preflight.Code, preflight.Header())
		}
	})

	t.Run("internal failure", func(t *testing.T) {
		callback := f.create(t, `{}`)
		bearer := f.create(t, `{}`)
		f.failControlOutbox(t)
		assertTokenHTTPError(t, f.serve(t, http.MethodPost, callbackPath(t, callback), "", `{"result":true}`), http.StatusInternalServerError, "internal_error", "internal server error")
		assertTokenHTTPError(t, f.bearerComplete(t, bearer.ID, bearer.PublicAccessToken, `{"result":true}`), http.StatusInternalServerError, "internal_error", "internal server error")
		if f.tokenStatus(t, callback.ID) != "pending" || f.tokenStatus(t, bearer.ID) != "pending" {
			t.Fatal("failed public completion changed a token")
		}
	})
}

func TestTokenListAndGetRoutes(t *testing.T) {
	f := newTokenHTTPFixture(t)
	first := f.create(t, `{"tags":["first"]}`)
	second := f.create(t, `{}`)
	decodeTokenStatus(t, f.serve(t, http.MethodPost, "/v1/tokens/"+second.ID+"/complete", f.key, `{"result":1}`), http.StatusOK)

	response := f.serve(t, http.MethodGet, "/v1/tokens?limit=1", f.key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", response.Code, response.Body.String())
	}
	var page api.ListTokensResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tokens) != 1 || page.Tokens[0].ID != second.ID || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	response = f.serve(t, http.MethodGet, "/v1/tokens?limit=1&cursor="+page.NextCursor, f.key, "")
	page = api.ListTokensResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK {
		t.Fatalf("second page = %d: %s", response.Code, response.Body.String())
	}
	if len(page.Tokens) != 1 || page.Tokens[0].ID != first.ID || page.NextCursor != "" ||
		len(page.Tokens[0].Tags) != 1 || page.Tokens[0].Tags[0] != "first" {
		t.Fatalf("second page = %+v", page)
	}
	response = f.serve(t, http.MethodGet, "/v1/tokens?status=completed", f.key, "")
	page = api.ListTokensResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK ||
		len(page.Tokens) != 1 || page.Tokens[0].ID != second.ID || page.Tokens[0].Status != api.TokenStatusCompleted {
		t.Fatalf("filtered list = %d: %s", response.Code, response.Body.String())
	}
	session := f.sessionToken(t)
	if response := f.serve(t, http.MethodGet, f.environmentPath()+"/tokens", session, ""); response.Code != http.StatusOK {
		t.Fatalf("session list = %d: %s", response.Code, response.Body.String())
	}
	for _, query := range []string{"?status=open", "?unknown=1", "?limit=1&limit=2", "?cursor=%21"} {
		if response := f.serve(t, http.MethodGet, "/v1/tokens"+query, f.key, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("list %s = %d: %s", query, response.Code, response.Body.String())
		}
	}
	assertTokenHTTPError(t, f.serve(t, http.MethodGet, "/v1/tokens", f.keyWith(t, auth.PermissionTokensCreate), ""),
		http.StatusForbidden, "forbidden", "permission is required")

	got := decodeTokenStatus(t, f.serve(t, http.MethodGet, "/v1/tokens/"+second.ID, f.key, ""), http.StatusOK)
	if got.ID != second.ID || got.Status != api.TokenStatusCompleted || string(got.Result) != `1` || got.PublicAccessToken != "" || got.CallbackURL != "" {
		t.Fatalf("get = %+v", got)
	}
	decodeTokenStatus(t, f.serve(t, http.MethodGet, f.environmentPath()+"/tokens/"+first.ID, session, ""), http.StatusOK)
	assertTokenHTTPError(t, f.serve(t, http.MethodGet, "/v1/tokens/"+uuid.NewV7().String(), f.key, ""),
		http.StatusNotFound, "token_not_found", "token was not found")
	assertTokenHTTPError(t, f.serve(t, http.MethodGet, "/v1/tokens/not-a-token", f.key, ""),
		http.StatusNotFound, "token_not_found", "token was not found")
	assertTokenHTTPError(t, f.serve(t, http.MethodGet, "/v1/tokens/"+first.ID, f.keyWith(t, auth.PermissionTokensCreate), ""),
		http.StatusForbidden, "forbidden", "permission is required")
	if response := f.serve(t, http.MethodGet, "/api/projects/not-a-project/environments/not-an-environment/tokens/"+first.ID, session, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope get = %d: %s", response.Code, response.Body.String())
	}
}

func TestTokenWaitCreateRoute(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, work.RunID)
	handler := newPostgresServer(t, f.Pool, withTokenPublicURL)
	worker := newWorkerHTTPClient(t, handler, f.Pool, f.WorkerID)
	key := issueEnvironmentAPIKey(t, f.Pool, f.OrgID, f.ProjectID, f.EnvironmentID, auth.PermissionTokensCreate)
	response := serveAPIKey(handler, http.MethodPost, "/v1/tokens", key, `{}`)
	token := decodeTokenStatus(t, response, http.StatusCreated)
	lease := workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1}
	wait := func(tokenID string) workerapi.CreateRunWaitRequest {
		return workerapi.CreateRunWaitRequest{
			Lease: lease, CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(),
			Kind: workerapi.RunWaitKindToken, Params: json.RawMessage(`{"token_id":"` + tokenID + `"}`),
		}
	}

	out := worker.post(t, "/worker/v1/run/waits/create", wait(uuid.NewV7().String()), http.StatusNotFound, nil)
	if body := decodeHTTPError(t, out.Body.Bytes()); body.Code != "token_not_found" || body.Message != "token was not found" {
		t.Fatalf("missing token = %s", out.Body.String())
	}
	// The lease is live, but a Task Run's wait carries an Actor input cursor,
	// so registration rejects its authority.
	actorCursor := wait(token.ID)
	zero := int64(0)
	actorCursor.ActorSpeculativeInputSequence = &zero
	out = worker.post(t, "/worker/v1/run/waits/create", actorCursor, http.StatusConflict, nil)
	if body := decodeHTTPError(t, out.Body.Bytes()); body.Code != "conflict" || body.Message != "worker run wait receipt is stale" {
		t.Fatalf("stale registration = %s", out.Body.String())
	}
	negative := wait(token.ID)
	cursor := int64(-1)
	negative.ActorSpeculativeInputSequence = &cursor
	out = worker.post(t, "/worker/v1/run/waits/create", negative, http.StatusInternalServerError, nil)
	if body := decodeHTTPError(t, out.Body.Bytes()); body.Code != "internal_error" || body.Message != "internal server error" {
		t.Fatalf("invalid registration = %s", out.Body.String())
	}

	request := wait(token.ID)
	var registered workerapi.CreateRunWaitResponse
	worker.post(t, "/worker/v1/run/waits/create", request, http.StatusOK, &registered)
	if registered.RunID != work.RunID.String() || registered.RunWaitID != request.RunWaitID ||
		registered.ResumeAttachID != request.ResumeAttachID || registered.ResolutionKind != "" {
		t.Fatalf("registered = %+v", registered)
	}
}

func TestPublicTokenCredentialLookupFailureIsInternal(t *testing.T) {
	for _, mode := range []string{"callback", "bearer"} {
		t.Run(mode, func(t *testing.T) {
			f := newTokenHTTPFixture(t)
			created := f.create(t, `{}`)
			table := "tokens"
			if mode == "bearer" {
				table = "public_access_tokens"
			}
			dbtest.MustExec(t, t.Context(), f.run.Pool, "ALTER TABLE "+table+" RENAME TO unavailable_"+table)
			var response *httptest.ResponseRecorder
			if mode == "callback" {
				response = f.serve(t, http.MethodPost, callbackPath(t, created), "", `{"result":true}`)
			} else {
				response = f.bearerComplete(t, created.ID, created.PublicAccessToken, `{"result":true}`)
			}
			assertTokenHTTPError(t, response, http.StatusInternalServerError, "internal_error", "internal server error")
		})
	}
}

func TestPublicTokenUsageWriteFailureRollsBackCompletion(t *testing.T) {
	f := newTokenHTTPFixture(t)
	created := f.create(t, `{}`)
	dbtest.MustExec(t, t.Context(), f.run.Pool, `CREATE FUNCTION reject_credential_usage() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'credential usage storage failed'; END $$`)
	dbtest.MustExec(t, t.Context(), f.run.Pool, `CREATE TRIGGER reject_credential_usage BEFORE UPDATE ON public_access_tokens FOR EACH ROW EXECUTE FUNCTION reject_credential_usage()`)
	assertTokenHTTPError(t, f.bearerComplete(t, created.ID, created.PublicAccessToken, `{"result":true}`), http.StatusInternalServerError, "internal_error", "internal server error")
	if status := f.tokenStatus(t, created.ID); status != "pending" {
		t.Fatalf("failed usage write committed Token status=%s", status)
	}
	var used int
	if err := f.run.Pool.QueryRow(t.Context(), `SELECT used_count FROM public_access_tokens WHERE token_id=$1`, created.ID).Scan(&used); err != nil || used != 0 {
		t.Fatalf("usage count=%d: %v", used, err)
	}
}
