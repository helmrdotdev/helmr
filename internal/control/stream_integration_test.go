package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	cellpkg "github.com/helmrdotdev/helmr/internal/cell"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type streamTokenRouteFixture struct {
	orgID            uuid.UUID
	projectID        uuid.UUID
	environmentID    uuid.UUID
	deploymentID     uuid.UUID
	deploymentTaskID uuid.UUID
	workerGroupID    uuid.UUID
	workspaceID      uuid.UUID
	sessionID        uuid.UUID
	inputStreamID    uuid.UUID
	outputStreamID   uuid.UUID
}

func streamTestPublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	id, err := newPublicID(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStreamsAndTokensRoutesWithAuthBoundaries(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	queries := db.New(pool)
	authSecret := []byte("abcdefghijabcdefghijabcdefghij12")
	handler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBTX:       pool,
		Auth:       fakeAuth{projectID: ids.projectID.String(), environmentID: ids.environmentID.String(), permissions: streamTokenRoutePermissions()},
		AuthSecret: authSecret,
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	readHandler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBTX:       pool,
		Auth:       fakeAuth{projectID: ids.projectID.String(), environmentID: ids.environmentID.String(), permissions: append(streamTokenRoutePermissions(), auth.PermissionRunsRead)},
		AuthSecret: authSecret,
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})

	rec := routeRequest(t, readHandler, http.MethodGet, "/api/sessions?external_id=slack%3AT123%3AC456", "", "Bearer machine-key")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ids.sessionID.String()) {
		t.Fatalf("external id session list status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, readHandler, http.MethodGet, "/api/sessions?external_id=missing", "", "Bearer machine-key")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Fatalf("missing external id session list status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, readHandler, http.MethodGet, "/api/sessions/by-external-id?external_id=slack%3AT123%3AC456", "", "Bearer machine-key")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ids.sessionID.String()) {
		t.Fatalf("external id session get status=%d body=%s", rec.Code, rec.Body.String())
	}
	orgScopedHandler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBTX:       pool,
		Auth:       fakeAuth{kind: auth.ActorKindSystem},
		AuthSecret: authSecret,
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	rec = routeRequest(t, orgScopedHandler, http.MethodGet, "/api/sessions/by-external-id?external_id=slack%3AT123%3AC456", "", "Bearer system")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unscoped external id session get status=%d body=%s", rec.Code, rec.Body.String())
	}
	sessionHandler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBTX:       pool,
		Auth:       fakeAuth{kind: auth.ActorKindSession},
		AuthSecret: authSecret,
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})

	inputBody := `{"data":{"approved":true},"correlation_id":"thread-1","idempotency_key":"input-1"}`
	rec = routeRequest(t, handler, http.MethodPost, "/api/sessions/by-external-id/inputs/approval?external_id=slack%3AT123%3AC456", inputBody, "Bearer machine-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("input send status=%d body=%s", rec.Code, rec.Body.String())
	}
	var appended api.AppendStreamRecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &appended); err != nil {
		t.Fatal(err)
	}
	if appended.Record.Sequence != 1 || appended.Record.StreamID != ids.inputStreamID.String() {
		t.Fatalf("input append response = %+v", appended)
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/sessions/"+ids.sessionID.String()+"/inputs/approval", `{"data":{"approved":true},"correlation_id":"thread-2","idempotency_key":"input-1"}`, "Bearer machine-key")
	if rec.Code != http.StatusConflict {
		t.Fatalf("input idempotency correlation conflict status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "idempotency_fingerprint_mismatch")

	rec = routeRequest(t, handler, http.MethodGet, "/api/sessions/"+ids.sessionID.String()+"/inputs/approval?correlation_id=thread-1", "", "Bearer machine-key")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"approved":true`) || !strings.Contains(rec.Body.String(), `"correlation_id":"thread-1"`) {
		t.Fatalf("input list status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, handler, http.MethodGet, "/api/sessions/"+ids.sessionID.String()+"/inputs/approval?correlation_id=thread-2", "", "Bearer machine-key")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"approved":true`) {
		t.Fatalf("input list wrong correlation status=%d body=%s", rec.Code, rec.Body.String())
	}
	seedControlDeploymentStream(t, ctx, pool, ids, ids.deploymentID, "updates", "input", "schema-updates-input", `{"kind":"input-updates"}`)
	rec = routeRequest(t, handler, http.MethodGet, "/api/sessions/"+ids.sessionID.String()+"/streams", "", "Bearer machine-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listedStreams api.ListSessionStreamsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listedStreams); err != nil {
		t.Fatal(err)
	}
	if !hasListedStream(listedStreams.Streams, "updates", "input") {
		t.Fatalf("stream list did not materialize deployment-only input stream: %+v", listedStreams.Streams)
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/sessions/"+ids.sessionID.String()+"/inputs/updates", inputBody, "Bearer machine-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("same-name input append status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sameNameInput api.AppendStreamRecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sameNameInput); err != nil {
		t.Fatal(err)
	}
	if sameNameInput.Record.StreamID == ids.outputStreamID.String() {
		t.Fatalf("same-name input used output stream id %s", ids.outputStreamID)
	}
	var deploymentSchemaJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT schema_json
		  FROM deployment_streams
		 WHERE org_id = $1
		   AND deployment_id = $2
		   AND name = 'updates'
		   AND direction = 'input'
	`, ids.orgID, ids.deploymentID).Scan(&deploymentSchemaJSON); err != nil {
		t.Fatal(err)
	}
	if string(deploymentSchemaJSON) != `{"kind": "input-updates"}` && string(deploymentSchemaJSON) != `{"kind":"input-updates"}` {
		t.Fatalf("deployment stream schema_json = %s", deploymentSchemaJSON)
	}

	rec = routeRequest(t, handler, http.MethodPost, "/api/sessions/"+ids.sessionID.String()+"/inputs/undeclared", inputBody, "Bearer machine-key")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("undeclared input append status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "stream_not_found")

	outputBody := `{"data":{"text":"ready"}}`
	rec = routeRequest(t, handler, http.MethodPost, "/api/sessions/"+ids.sessionID.String()+"/outputs/updates", outputBody, "Bearer machine-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("output append status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, handler, http.MethodGet, "/api/sessions/"+ids.sessionID.String()+"/outputs/updates/read", "", "Bearer machine-key")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"ready"`) {
		t.Fatalf("output read status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/public-access-tokens", `{"scope":{"type":"session.input.send","session":{"type":"external_id","external_id":"slack:T123:C456"},"stream":"approval","correlation_id":"thread-3"},"max_uses":2}`, "Bearer machine-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("public input token create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var publicInput api.PublicAccessTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &publicInput); err != nil {
		t.Fatal(err)
	}
	if publicInput.PublicAccessToken == "" || publicInput.Scope.Type != "session.input.send" || publicInput.Scope.CorrelationID != "thread-3" {
		t.Fatalf("public input token response = %+v", publicInput)
	}
	orgScopedAPIKeyHandler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBTX:       pool,
		Auth:       fakeAuth{kind: auth.ActorKindAPIKey},
		AuthSecret: authSecret,
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	rec = routeRequest(t, orgScopedAPIKeyHandler, http.MethodPost, "/api/public-access-tokens", `{"scope":{"type":"session.input.send","session":{"type":"id","id":"`+ids.sessionID.String()+`"},"stream":"approval"}}`, "Bearer org-key")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("org-scoped api key public token create status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, sessionHandler, http.MethodPost, "/api/public-access-tokens", `{"scope":{"type":"session.input.send","session":{"type":"id","id":"`+ids.sessionID.String()+`"},"stream":"approval"}}`, "Bearer user-session")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("session actor public token create without scope status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, sessionHandler, http.MethodPost, "/api/public-access-tokens", `{"project_id":"`+ids.projectID.String()+`","scope":{"type":"session.input.send","session":{"type":"id","id":"`+ids.sessionID.String()+`"},"stream":"approval"}}`, "Bearer user-session")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("session actor public token create with partial scope status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, sessionHandler, http.MethodPost, "/api/public-access-tokens", `{"project_id":"`+ids.projectID.String()+`","environment_id":"`+ids.environmentID.String()+`","scope":{"type":"session.input.send","session":{"type":"external_id","external_id":"slack:T123:C456"},"stream":"approval","correlation_id":"thread-4"},"max_uses":1}`, "Bearer user-session")
	if rec.Code != http.StatusCreated {
		t.Fatalf("user public input token create with external id status=%d body=%s", rec.Code, rec.Body.String())
	}
	var publicInputFromSessionActor api.PublicAccessTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &publicInputFromSessionActor); err != nil {
		t.Fatal(err)
	}
	if publicInputFromSessionActor.Scope.Session.Type != "id" || publicInputFromSessionActor.Scope.Session.ID != ids.sessionID.String() || publicInputFromSessionActor.Scope.CorrelationID != "thread-4" {
		t.Fatalf("user public input token response = %+v", publicInputFromSessionActor)
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/v1/sessions/by-external-id/inputs/approval?external_id=slack%3AT123%3AC456", `{"data":{"approved":false},"correlation_id":"thread-2"}`, "Bearer "+publicInput.PublicAccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public input wrong correlation status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "token_scope_denied")
	otherSessionID, _ := seedControlStreamSession(t, ctx, pool, ids, "slack:T123:C999", "approval", "input")
	rec = routeRequest(t, handler, http.MethodPost, "/api/v1/sessions/"+otherSessionID.String()+"/inputs/approval", `{"data":{"approved":false},"correlation_id":"thread-3"}`, "Bearer "+publicInput.PublicAccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public input different session status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "token_scope_denied")
	rec = routeRequest(t, handler, http.MethodPost, "/api/v1/sessions/by-external-id/inputs/approval?external_id=slack%3AT123%3AC456", `{"data":{"approved":false},"correlation_id":"thread-3"}`, "Bearer "+publicInput.PublicAccessToken)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"approved":false`) {
		t.Fatalf("public input send status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/public-access-tokens", `{"scope":{"type":"session.output.read","session":{"type":"id","id":"`+ids.sessionID.String()+`"},"stream":"updates"},"max_uses":1}`, "Bearer machine-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("public output token create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var publicOutput api.PublicAccessTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &publicOutput); err != nil {
		t.Fatal(err)
	}
	rec = routeRequest(t, handler, http.MethodGet, "/api/v1/sessions/"+ids.sessionID.String()+"/outputs/updates/read", "", "Bearer "+publicOutput.PublicAccessToken)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"ready"`) {
		t.Fatalf("public output read status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = routeRequest(t, handler, http.MethodGet, "/api/v1/sessions/"+ids.sessionID.String()+"/outputs/updates/read", "", "Bearer "+publicOutput.PublicAccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public output replay status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "token_scope_denied")

	rec = routeRequest(t, handler, http.MethodPost, "/api/tokens", `{"timeout":"1h","idempotency_key":"token-1","metadata":{"kind":"approval"},"tags":["release"]}`, "Bearer machine-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("token create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var token api.TokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	if token.ID == "" || token.PublicAccessToken == "" || !strings.Contains(token.CallbackURL, "/api/v1/tokens/"+token.ID+"/callback/") {
		t.Fatalf("token response = %+v", token)
	}
	rec = routeRequest(t, handler, http.MethodGet, "/api/tokens/"+token.ID, "", "Bearer machine-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("token get status=%d body=%s", rec.Code, rec.Body.String())
	}
	var retrieved api.TokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &retrieved); err != nil {
		t.Fatal(err)
	}
	if retrieved.CallbackURL != "" || retrieved.PublicAccessToken != "" {
		t.Fatalf("token retrieve leaked completion capability: %+v", retrieved)
	}

	rec = routeRequest(t, handler, http.MethodPost, "/api/tokens/"+token.ID+"/complete", `{"data":{"ok":true}}`, "Bearer machine-key")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"completed"`) {
		t.Fatalf("api key complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	var apiComplete api.CompleteTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &apiComplete); err != nil {
		t.Fatal(err)
	}
	if apiComplete.Status != "completed" || apiComplete.Token.Status != "completed" || string(apiComplete.Token.Data) != `{"ok":true}` {
		t.Fatalf("api key complete response = %+v token=%+v", apiComplete, apiComplete.Token)
	}
	if strings.Contains(rec.Body.String(), "callback_url") || strings.Contains(rec.Body.String(), "public_access_token") {
		t.Fatalf("complete response leaked completion capability: %s", rec.Body.String())
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/tokens/"+token.ID+"/complete", `{"data":{"ok":true}}`, "Bearer machine-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("api key duplicate complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	var apiDuplicate api.CompleteTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &apiDuplicate); err != nil {
		t.Fatal(err)
	}
	if apiDuplicate.Status != "already_completed" || string(apiDuplicate.Token.Data) != `{"ok":true}` {
		t.Fatalf("api key duplicate response = %+v token=%+v", apiDuplicate, apiDuplicate.Token)
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/tokens/"+token.ID+"/complete", `{"data":{"ok":false}}`, "Bearer machine-key")
	if rec.Code != http.StatusConflict {
		t.Fatalf("completion conflict status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "token_completion_conflict")
	rec = routeRequest(t, handler, http.MethodPost, "/api/v1/tokens/"+token.ID+"/complete", `{"data":{"ok":false}}`, "Bearer "+token.PublicAccessToken)
	if rec.Code != http.StatusConflict {
		t.Fatalf("public completion conflict status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "token_completion_conflict")
	tokenHash, err := auth.HashToken(authSecret, token.PublicAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	publicAccessToken, err := queries.LockPublicAccessTokenByHash(ctx, tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if publicAccessToken.UsedCount != 0 {
		t.Fatalf("public completion conflict consumed access token: used_count=%d", publicAccessToken.UsedCount)
	}

	publicToken := createRouteToken(t, handler)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/tokens/"+publicToken.ID+"/complete", nil)
	req.Header.Set("origin", "https://helmr.example.test")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("access-control-allow-origin") != "https://helmr.example.test" {
		t.Fatalf("public completion preflight status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/v1/tokens/"+publicToken.ID+"/complete", `{"data":{"public":true}}`, "Bearer "+publicToken.PublicAccessToken)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"public":true`) {
		t.Fatalf("public complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	var publicComplete api.CompleteTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &publicComplete); err != nil {
		t.Fatal(err)
	}
	if publicComplete.Status != "completed" {
		t.Fatalf("public complete response = %+v", publicComplete)
	}
	publicTokenHash, err := auth.HashToken(authSecret, publicToken.PublicAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	consumedPublicToken, err := queries.LockPublicAccessTokenByHash(ctx, publicTokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if consumedPublicToken.UsedCount != 1 {
		t.Fatalf("public access token used_count=%d, want 1", consumedPublicToken.UsedCount)
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/v1/tokens/"+publicToken.ID+"/complete", `{"data":{"public":true}}`, "Bearer "+publicToken.PublicAccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("public token replay status=%d body=%s", rec.Code, rec.Body.String())
	}
	var publicDuplicate api.CompleteTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &publicDuplicate); err != nil {
		t.Fatal(err)
	}
	if publicDuplicate.Status != "already_completed" || string(publicDuplicate.Token.Data) != `{"public":true}` {
		t.Fatalf("public duplicate response = %+v token=%+v", publicDuplicate, publicDuplicate.Token)
	}
	replayedPublicToken, err := queries.LockPublicAccessTokenByHash(ctx, publicTokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if replayedPublicToken.UsedCount != 1 {
		t.Fatalf("public duplicate used_count=%d, want 1", replayedPublicToken.UsedCount)
	}

	wrongRaw := createPublicStreamScope(t, ctx, queries, authSecret, ids)
	conflictToken := createRouteToken(t, handler)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tokens/"+conflictToken.ID+"/complete", strings.NewReader(`{"data":{"ok":true}}`))
	req.Header.Set("authorization", "Bearer "+wrongRaw)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://helmr.example.test")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong public scope status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("access-control-allow-origin") != "https://helmr.example.test" {
		t.Fatalf("wrong public scope should include browser CORS header: %v", rec.Header())
	}
	requireErrorCode(t, rec.Body.Bytes(), "token_scope_denied")

	callbackToken := createRouteToken(t, handler)
	callbackURL, err := url.Parse(callbackToken.CallbackURL)
	if err != nil {
		t.Fatal(err)
	}
	rec = routeRequest(t, handler, http.MethodPost, callbackURL.Path, `{"data":{"callback":true}}`, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"callback":true`) {
		t.Fatalf("callback complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("access-control-allow-origin") != "" {
		t.Fatalf("callback route should not emit CORS headers: %v", rec.Header())
	}

	expiredToken, err := queries.CreateToken(ctx, db.CreateTokenParams{
		PublicID:                 streamTestPublicID(t, publicid.Token),
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		CellID:                   dbtest.DefaultCellID,
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(-time.Minute)),
		CreateRequestFingerprint: "expired-cancel",
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = routeRequest(t, handler, http.MethodPost, "/api/tokens/"+pgvalue.MustUUIDValue(expiredToken.ID).String()+"/cancel", `{}`, "Bearer machine-key")
	if rec.Code != http.StatusGone {
		t.Fatalf("expired token cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "token_expired")
}

func TestWorkerActiveInputReadDoesNotRequireWakeupTransportForBufferedRecord(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	queries := db.New(pool)
	worker, leaseIDs := seedControlRunningRunLease(t, ctx, pool, ids)
	if _, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		PublicID:               streamTestPublicID(t, publicid.StreamRecord),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               pgvalue.UUID(ids.inputStreamID),
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"ready":true}`),
		ContentType:            "application/json",
		IdempotencyFingerprint: "buffered-record",
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	}); err != nil {
		t.Fatal(err)
	}
	server := newControlIntegrationServer(pool)

	response, err := server.readWorkerInputStream(ctx, worker, leaseIDs, api.WorkerActiveStreamReadRequest{
		Stream:        "approval",
		AfterSequence: 0,
		Block:         false,
	})
	if err != nil {
		t.Fatal(err)
	}
	samePayload := false
	if response.Record != nil {
		samePayload, err = sameJSONValue(response.Record.Data, []byte(`{"ready":true}`))
		if err != nil {
			t.Fatal(err)
		}
	}
	if response.Record == nil || response.Record.Sequence != 1 || !samePayload {
		t.Fatalf("read response = %+v", response)
	}
}

func TestWorkerActiveInputReadSkipsAcceptedSessionRunRequest(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	queries := db.New(pool)
	worker, leaseIDs := seedControlRunningRunLease(t, ctx, pool, ids)
	recordID := uuid.Must(uuid.NewV7())
	if _, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		PublicID:               streamTestPublicID(t, publicid.StreamRecord),
		ID:                     pgvalue.UUID(recordID),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               pgvalue.UUID(ids.inputStreamID),
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"ready":true}`),
		ContentType:            "application/json",
		IdempotencyFingerprint: "accepted-request-record",
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	}); err != nil {
		t.Fatal(err)
	}
	request, err := queries.EnsureSessionRunRequestForStreamRecord(ctx, db.EnsureSessionRunRequestForStreamRecordParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:          pgvalue.UUID(ids.orgID),
		CellID:         dbtest.DefaultCellID,
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		SessionID:      pgvalue.UUID(ids.sessionID),
		StreamRecordID: pgvalue.UUID(recordID),
		StreamID:       pgvalue.UUID(ids.inputStreamID),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := newControlIntegrationServer(pool)

	response, err := server.readWorkerInputStream(ctx, worker, leaseIDs, api.WorkerActiveStreamReadRequest{
		Stream:        "approval",
		AfterSequence: 0,
		Block:         false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Record == nil || response.Record.ID != recordID.String() {
		t.Fatalf("read response = %+v", response)
	}
	stored, err := queries.GetSessionRunRequest(ctx, db.GetSessionRunRequestParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            request.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "skipped" || stored.LastError != "consumed_by_active_run" {
		t.Fatalf("request status=%q last_error=%q, want skipped consumed_by_active_run", stored.Status, stored.LastError)
	}
}

func TestWorkerActiveInputReadCancelsCreatedSessionRunRequest(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	queries := db.New(pool)
	worker, leaseIDs := seedControlRunningRunLease(t, ctx, pool, ids)
	recordID := uuid.Must(uuid.NewV7())
	if _, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		PublicID:               streamTestPublicID(t, publicid.StreamRecord),
		ID:                     pgvalue.UUID(recordID),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               pgvalue.UUID(ids.inputStreamID),
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"ready":true}`),
		ContentType:            "application/json",
		IdempotencyFingerprint: "created-request-record",
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	}); err != nil {
		t.Fatal(err)
	}
	request, err := queries.EnsureSessionRunRequestForStreamRecord(ctx, db.EnsureSessionRunRequestForStreamRecordParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:          pgvalue.UUID(ids.orgID),
		CellID:         dbtest.DefaultCellID,
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		SessionID:      pgvalue.UUID(ids.sessionID),
		StreamRecordID: pgvalue.UUID(recordID),
		StreamID:       pgvalue.UUID(ids.inputStreamID),
	})
	if err != nil {
		t.Fatal(err)
	}
	continuationRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, 'approval-task', $9, 'queued', 'queued', '{}', 'default', 300000,
			'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'bbbbbbbbbbbbbbbb')
	`, continuationRunID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, ids.deploymentTaskID, ids.workspaceID, ids.sessionID, streamTestPublicID(t, publicid.Run)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_runs (id, public_id, org_id, cell_id, project_id, environment_id, session_id, run_id, deployment_id, previous_run_id, turn_index, reason)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, $9, 1, 'input')
	`, uuid.Must(uuid.NewV7()), ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.sessionID, continuationRunID, ids.deploymentID, leaseIDs.runID, streamTestPublicID(t, publicid.SessionRun)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE session_run_requests
		   SET status = 'created',
		       run_id = $1
		 WHERE org_id = $2
		   AND project_id = $3
		   AND environment_id = $4
		   AND id = $5
	`, continuationRunID, ids.orgID, ids.projectID, ids.environmentID, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sessions
		   SET current_run_id = $1
		 WHERE org_id = $2
		   AND project_id = $3
		   AND environment_id = $4
		   AND id = $5
	`, continuationRunID, ids.orgID, ids.projectID, ids.environmentID, ids.sessionID); err != nil {
		t.Fatal(err)
	}
	server := newControlIntegrationServer(pool)

	response, err := server.readWorkerInputStream(ctx, worker, leaseIDs, api.WorkerActiveStreamReadRequest{
		Stream:        "approval",
		AfterSequence: 0,
		Block:         false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Record == nil || response.Record.ID != recordID.String() {
		t.Fatalf("read response = %+v", response)
	}
	stored, err := queries.GetSessionRunRequest(ctx, db.GetSessionRunRequestParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            request.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "skipped" || stored.LastError != "consumed_by_active_run" {
		t.Fatalf("request status=%q last_error=%q, want skipped consumed_by_active_run", stored.Status, stored.LastError)
	}
	var runStatus db.RunStatus
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status, execution_status FROM runs WHERE org_id = $1 AND id = $2`, ids.orgID, continuationRunID).Scan(&runStatus, &executionStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusCancelled || executionStatus != db.RunExecutionStatusFinished {
		t.Fatalf("continuation run status=%s execution_status=%s, want cancelled finished", runStatus, executionStatus)
	}
	var currentRunID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT current_run_id FROM sessions WHERE org_id = $1 AND id = $2`, ids.orgID, ids.sessionID).Scan(&currentRunID); err != nil {
		t.Fatal(err)
	}
	if currentRunID != leaseIDs.runID {
		t.Fatalf("current_run_id=%s, want active run %s", currentRunID, leaseIDs.runID)
	}
}

func TestWorkerActiveInputReadDoesNotSkipCreatedRequestForActiveRun(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	queries := db.New(pool)
	worker, leaseIDs := seedControlRunningRunLease(t, ctx, pool, ids)
	recordID := uuid.Must(uuid.NewV7())
	if _, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		PublicID:               streamTestPublicID(t, publicid.StreamRecord),
		ID:                     pgvalue.UUID(recordID),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               pgvalue.UUID(ids.inputStreamID),
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"ready":true}`),
		ContentType:            "application/json",
		IdempotencyFingerprint: "created-active-request-record",
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	}); err != nil {
		t.Fatal(err)
	}
	request, err := queries.EnsureSessionRunRequestForStreamRecord(ctx, db.EnsureSessionRunRequestForStreamRecordParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:          pgvalue.UUID(ids.orgID),
		CellID:         dbtest.DefaultCellID,
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		SessionID:      pgvalue.UUID(ids.sessionID),
		StreamRecordID: pgvalue.UUID(recordID),
		StreamID:       pgvalue.UUID(ids.inputStreamID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE session_run_requests
		   SET status = 'created',
		       run_id = $1
		 WHERE org_id = $2
		   AND project_id = $3
		   AND environment_id = $4
		   AND id = $5
	`, leaseIDs.runID, ids.orgID, ids.projectID, ids.environmentID, request.ID); err != nil {
		t.Fatal(err)
	}
	server := newControlIntegrationServer(pool)

	response, err := server.readWorkerInputStream(ctx, worker, leaseIDs, api.WorkerActiveStreamReadRequest{
		Stream:        "approval",
		AfterSequence: 0,
		Block:         false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Record == nil || response.Record.ID != recordID.String() {
		t.Fatalf("read response = %+v", response)
	}
	stored, err := queries.GetSessionRunRequest(ctx, db.GetSessionRunRequestParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            request.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "created" || pgvalue.MustUUIDValue(stored.RunID) != leaseIDs.runID {
		t.Fatalf("request status=%q run_id=%s, want created active run %s", stored.Status, pgvalue.UUIDString(stored.RunID), leaseIDs.runID)
	}
	var runStatus db.RunStatus
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status, execution_status FROM runs WHERE org_id = $1 AND id = $2`, ids.orgID, leaseIDs.runID).Scan(&runStatus, &executionStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusRunning || executionStatus != db.RunExecutionStatusExecuting {
		t.Fatalf("active run status=%s execution_status=%s, want running executing", runStatus, executionStatus)
	}
}

func TestWorkerActiveInputReadRequiresWakeupTransportOnlyWhenBlockingAfterMiss(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	worker, leaseIDs := seedControlRunningRunLease(t, ctx, pool, ids)
	server := newControlIntegrationServer(pool)

	response, err := server.readWorkerInputStream(ctx, worker, leaseIDs, api.WorkerActiveStreamReadRequest{
		Stream:        "approval",
		AfterSequence: 0,
		Block:         false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.TimedOut {
		t.Fatalf("non-blocking miss response = %+v, want timeout", response)
	}
	timeoutSeconds := int32(1)
	_, err = server.readWorkerInputStream(ctx, worker, leaseIDs, api.WorkerActiveStreamReadRequest{
		Stream:         "approval",
		AfterSequence:  0,
		Block:          true,
		TimeoutSeconds: &timeoutSeconds,
	})
	if err == nil || !strings.Contains(err.Error(), "active stream transport unavailable") {
		t.Fatalf("blocking miss err = %v, want active stream unavailable", err)
	}
}

func TestWorkerActiveInputReadRechecksDBAfterWakeupCursorInitialization(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	queries := db.New(pool)
	worker, leaseIDs := seedControlRunningRunLease(t, ctx, pool, ids)
	server := newControlIntegrationServer(pool)
	wakeups := &cursorInitAppendWakeups{
		t:       t,
		queries: queries,
		ids:     ids,
	}
	timeoutSeconds := int32(5)
	response, err := server.readWorkerInputStreamWithWakeups(ctx, worker, leaseIDs, api.WorkerActiveStreamReadRequest{
		Stream:         "approval",
		AfterSequence:  0,
		Block:          true,
		TimeoutSeconds: &timeoutSeconds,
	}, wakeups)
	if err != nil {
		t.Fatal(err)
	}
	samePayload := false
	if response.Record != nil {
		samePayload, err = sameJSONValue(response.Record.Data, []byte(`{"race":true}`))
		if err != nil {
			t.Fatal(err)
		}
	}
	if response.Record == nil || response.Record.Sequence != 1 || !samePayload {
		t.Fatalf("read response = %+v", response)
	}
	if wakeups.waitCalled {
		t.Fatal("active stream read called XREAD wait after cursor initialization instead of rechecking DB")
	}
}

type cursorInitAppendWakeups struct {
	t          *testing.T
	queries    *db.Queries
	ids        streamTokenRouteFixture
	waitCalled bool
}

func (w *cursorInitAppendWakeups) latestSessionInputStreamWakeupID(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID) (string, error) {
	w.t.Helper()
	if _, err := w.queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		PublicID:               streamTestPublicID(w.t, publicid.StreamRecord),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(w.ids.orgID),
		CellID:                 dbtest.DefaultCellID,
		ProjectID:              pgvalue.UUID(w.ids.projectID),
		EnvironmentID:          pgvalue.UUID(w.ids.environmentID),
		StreamID:               streamID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"race":true}`),
		ContentType:            "application/json",
		IdempotencyFingerprint: "cursor-init-race",
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	}); err != nil {
		w.t.Fatal(err)
	}
	return "1-0", nil
}

func (w *cursorInitAppendWakeups) waitSessionInputStreamWakeup(context.Context, pgtype.UUID, pgtype.UUID, string, time.Duration) (string, error) {
	w.waitCalled = true
	return "", errActiveStreamUnavailable
}

func routeRequest(t *testing.T, handler http.Handler, method string, path string, body string, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if body != "" {
		req.Header.Set("content-type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("authorization", authorization)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func hasListedStream(streams []api.StreamResponse, name string, direction string) bool {
	for _, stream := range streams {
		if stream.Name == name && stream.Direction == direction {
			return true
		}
	}
	return false
}

func createRouteToken(t *testing.T, handler http.Handler) api.TokenResponse {
	t.Helper()
	rec := routeRequest(t, handler, http.MethodPost, "/api/tokens", `{"timeout":"1h"}`, "Bearer machine-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("token create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var token api.TokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	return token
}

func createPublicStreamScope(t *testing.T, ctx context.Context, queries *db.Queries, authSecret []byte, ids streamTokenRouteFixture) string {
	t.Helper()
	raw := "stream-public-token"
	hash, err := auth.HashToken(authSecret, raw)
	if err != nil {
		t.Fatal(err)
	}
	publicToken, err := queries.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		PublicID:      streamTestPublicID(t, publicid.PublicAccessToken),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		TokenHash:     hash,
		ExpiresAt:     pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		Metadata:      []byte(`{}`),
		CreatedBy:     []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(ids.orgID),
		CellID:              dbtest.DefaultCellID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeSessioninputsend,
		StreamID:            pgvalue.UUID(ids.inputStreamID),
	}); err != nil {
		t.Fatal(err)
	}
	return raw
}

func streamTokenRoutePermissions() []auth.Permission {
	return []auth.Permission{
		auth.PermissionSessionStreamsRead,
		auth.PermissionSessionInputSend,
		auth.PermissionSessionOutputAppend,
		auth.PermissionTokensCreate,
		auth.PermissionTokensRead,
		auth.PermissionTokensComplete,
		auth.PermissionTokensCancel,
	}
}

func newControlIntegrationDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("HELMR_TEST_DATABASE_URL is not set")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var serverVersion int
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		admin.Close()
		t.Skipf("Postgres %d does not provide uuidv7(); skipping integration test", serverVersion)
	}
	name := "helmr_control_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	testDSN := controlTestDatabaseDSN(t, dsn, name)
	if err := schema.Up(ctx, testDSN); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := cellpkg.Bootstrap(ctx, db.New(pool), cellpkg.BootstrapConfig{
		RegionID:          dbtest.DefaultRegionID,
		DefaultRegionID:   dbtest.DefaultRegionID,
		Provider:          dbtest.DefaultProvider,
		ProviderRegion:    dbtest.DefaultProviderRegion,
		RegionDisplayName: dbtest.DefaultRegionDisplay,
		CellID:            dbtest.DefaultCellID,
		EnvironmentClass:  dbtest.DefaultEnvironmentClass,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cellpkg.ReportHealth(ctx, db.New(pool), cellpkg.HealthConfig{
		CellID:             dbtest.DefaultCellID,
		Component:          cellpkg.ComponentDispatcher,
		RequiredComponents: cellpkg.RoutingRequiredComponents(),
	}); err != nil {
		t.Fatal(err)
	}
	return pool
}

func controlTestDatabaseDSN(t *testing.T, dsn string, database string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func newControlIntegrationServer(pool *pgxpool.Pool) *Server {
	return &Server{
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:              db.New(pool),
		tx:              pool,
		cellID:          dbtest.DefaultCellID,
		regionID:        dbtest.DefaultRegionID,
		defaultRegionID: dbtest.DefaultRegionID,
	}
}

func seedControlStreamTokenFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) streamTokenRouteFixture {
	t.Helper()
	ids := streamTokenRouteFixture{
		orgID:            dbtest.DefaultOrgID,
		projectID:        testProjectIDStringUUID(),
		environmentID:    testEnvironmentIDStringUUID(),
		deploymentID:     uuid.Must(uuid.NewV7()),
		deploymentTaskID: uuid.Must(uuid.NewV7()),
		workerGroupID:    uuid.Must(uuid.NewV7()),
		workspaceID:      uuid.Must(uuid.NewV7()),
		sessionID:        uuid.Must(uuid.NewV7()),
		inputStreamID:    uuid.Must(uuid.NewV7()),
		outputStreamID:   uuid.Must(uuid.NewV7()),
	}
	artifactID := uuid.Must(uuid.NewV7())
	sandboxID := uuid.Must(uuid.NewV7())
	taskBundleID := uuid.Must(uuid.NewV7())
	taskID := "approval-task"
	digest := "sha256:" + strings.Repeat("1", 64)
	rootfsDigest := "sha256:" + strings.Repeat("2", 64)

	if _, err := pool.Exec(ctx, `INSERT INTO organizations (id, public_id, name, slug) VALUES ($1, $2, 'Default', 'default')`, ids.orgID, streamTestPublicID(t, publicid.Organization)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name) VALUES ($1, $4, $2, $3, 'proj', 'Project')`, ids.projectID, ids.orgID, dbtest.DefaultRegionID, streamTestPublicID(t, publicid.Project)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO environments (id, public_id, org_id, project_id, default_region_id, slug, name, color_hex) VALUES ($1, $5, $2, $3, $4, 'env', 'Env', '#3366ff')`, ids.environmentID, ids.orgID, ids.projectID, dbtest.DefaultRegionID, streamTestPublicID(t, publicid.Environment)); err != nil {
		t.Fatal(err)
	}
	queries := db.New(pool)
	if _, err := queries.EnsureOrgCell(ctx, db.EnsureOrgCellParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		CellID: dbtest.DefaultCellID,
		Role:   db.OrgCellRoleHome,
		State:  db.OrgCellStateActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cellpkg.EnsureEnvironmentRoute(ctx, queries, cellpkg.EnsureEnvironmentRouteParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RegionID:      dbtest.DefaultRegionID,
		LocalCellID:   dbtest.DefaultCellID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO worker_groups (id, cell_id, name) VALUES ($1, $2, 'test')`, ids.workerGroupID, dbtest.DefaultCellID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO cas_objects (org_id, cell_id, digest, size_bytes, media_type) VALUES ($1, $2, $3, 1, 'application/octet-stream'), ($1, $2, $4, 1, 'application/octet-stream')`, ids.orgID, dbtest.DefaultCellID, digest, rootfsDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO artifacts (id, org_id, cell_id, project_id, environment_id, digest, kind, size_bytes, media_type) VALUES ($1, $2, $3, $4, $5, $6, 'task_bundle', 1, 'application/octet-stream')`, artifactID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO artifacts (id, org_id, cell_id, project_id, environment_id, digest, kind, size_bytes, media_type) VALUES ($1, $2, $3, $4, $5, $6, 'sandbox_image', 1, 'application/octet-stream')`, taskBundleID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, rootfsDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deployments (id, public_id, org_id, cell_id, project_id, environment_id, worker_group_id, version, content_hash, deployment_source_artifact_id, status) VALUES ($1, $9, $2, $3, $4, $5, $6, 'v1', $7, $8, 'deployed')`, ids.deploymentID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.workerGroupID, digest, artifactID, streamTestPublicID(t, publicid.Deployment)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deployment_queues (org_id, cell_id, project_id, environment_id, deployment_id, name) VALUES ($1, $2, $3, $4, $5, 'default')`, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks (public_id, org_id, cell_id, project_id, environment_id, task_id) VALUES ($6, $1, $2, $3, $4, $5)`, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, taskID, streamTestPublicID(t, publicid.Task)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deployment_sandboxes (id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, sandbox_id, image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format, workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format, contract_version, fingerprint) VALUES ($1, $9, $2, $3, $4, $5, $6, 'default', $7, 'oci-tar', $8, $8, 'oci-tar', '/workspace', 'test', 'guestd-test', 'adapter-test', 'tar', 1, 'sandbox-fingerprint')`, sandboxID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, taskBundleID, rootfsDigest, streamTestPublicID(t, publicid.Sandbox)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deployment_tasks (id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_sandbox_id, task_id, bundle_artifact_id, queue_name, max_active_duration_ms) VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, $9, 'default', 300000)`, ids.deploymentTaskID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, sandboxID, taskID, artifactID, streamTestPublicID(t, publicid.DeploymentTask)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, public_id, org_id, cell_id, project_id, environment_id, deployment_sandbox_id, sandbox_id, sandbox_fingerprint) VALUES ($1, $7, $2, $3, $4, $5, $6, 'default', 'sandbox-fingerprint')`, ids.workspaceID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, sandboxID, streamTestPublicID(t, publicid.Workspace)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, public_id, org_id, cell_id, project_id, environment_id, task_id, initial_deployment_id, active_deployment_id, workspace_id, external_id) VALUES ($1, $9, $2, $3, $4, $5, $6, $7, $7, $8, 'slack:T123:C456')`, ids.sessionID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, taskID, ids.deploymentID, ids.workspaceID, streamTestPublicID(t, publicid.Session)); err != nil {
		t.Fatal(err)
	}
	seedControlStream(t, ctx, pool, ids, ids.deploymentID, ids.inputStreamID, "approval", "input")
	seedControlStream(t, ctx, pool, ids, ids.deploymentID, ids.outputStreamID, "updates", "output")
	return ids
}

func seedControlRunningRunLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids streamTokenRouteFixture) (workerActor, workerRunLeaseIDs) {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	runLeaseID := uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	dispatchMessageID := "dispatch-" + runLeaseID.String()[:8]
	dispatchLeaseID := "lease-" + runLeaseID.String()[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, org_id, cell_id, resource_id, total_milli_cpu, total_memory_mib, total_disk_mib,
			worker_group_id, protocol_version,
			total_execution_slots, available_milli_cpu, available_memory_mib, available_disk_mib,
			available_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, 1000, 1024, 4096, $5, $6, 1, 1000, 1024, 4096, 1,
			$7, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultCellID, "worker-"+workerID.String()[:8], ids.workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, 'approval-task', $9, 'running', 'executing', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, runID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, ids.deploymentTaskID, ids.workspaceID, ids.sessionID, streamTestPublicID(t, publicid.Run)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, cell_id, worker_group_id, requested_milli_cpu, requested_memory_mib,
			requested_disk_mib, requested_execution_slots, runtime_id, runtime_arch,
			runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, 1000, 1024, 4096, 1, $5, 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runID, ids.orgID, dbtest.DefaultCellID, ids.workerGroupID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, cell_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'running')
	`, attemptID, ids.orgID, dbtest.DefaultCellID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, cell_id, run_id, attempt_id, worker_instance_id, worker_group_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 'running', now() + interval '1 hour', $10,
			'11111111111111111111111111111111', '3333333333333333', '2222222222222222',
			'00-11111111111111111111111111111111-3333333333333333-01')
	`, runLeaseID, ids.orgID, dbtest.DefaultCellID, runID, attemptID, workerID, ids.workerGroupID, dispatchMessageID, dispatchLeaseID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET current_run_lease_id = $1,
		       current_attempt_id = $2,
		       current_attempt_number = 1,
		       active_started_at = now()
		 WHERE org_id = $3
		   AND id = $4
	`, runLeaseID, attemptID, ids.orgID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sessions
		   SET current_run_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, runID, ids.orgID, ids.sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, cell_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, $3, 'reserved', 'default', $4, $5, now() + interval '1 hour')
	`, runID, ids.orgID, dbtest.DefaultCellID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}
	return workerActor{
			WorkerInstanceID: workerID,
			WorkerGroupID:    ids.workerGroupID,
			CellID:           dbtest.DefaultCellID,
			ResourceID:       "worker-" + workerID.String()[:8],
		}, workerRunLeaseIDs{
			orgID:           ids.orgID,
			runLeaseID:      runLeaseID,
			runID:           runID,
			protocolVersion: api.CurrentWorkerProtocolVersion,
			attemptNumber:   1,
			queueMessageID:  dispatchMessageID,
			queueLeaseID:    dispatchLeaseID,
		}
}

func seedControlStream(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids streamTokenRouteFixture, deploymentID uuid.UUID, streamID uuid.UUID, name string, direction string) {
	t.Helper()
	deploymentStreamID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO deployment_streams (id, org_id, cell_id, project_id, environment_id, deployment_id, name, direction, schema_fingerprint, schema_json) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '{}')`, deploymentStreamID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, deploymentID, name, direction, "schema-"+name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO streams (id, public_id, org_id, cell_id, project_id, environment_id, session_id, deployment_stream_id, name, direction, schema_fingerprint) VALUES ($1, $11, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, streamID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.sessionID, deploymentStreamID, name, direction, "schema-"+name, streamTestPublicID(t, publicid.Stream)); err != nil {
		t.Fatal(err)
	}
}

func seedControlStreamSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids streamTokenRouteFixture, externalID string, streamName string, direction string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	sessionID := uuid.Must(uuid.NewV7())
	streamID := uuid.Must(uuid.NewV7())
	var deploymentStreamID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id
		  FROM deployment_streams
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND deployment_id = $4
		   AND name = $5
		   AND direction = $6
	`, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, streamName, direction).Scan(&deploymentStreamID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (
			id, public_id, org_id, cell_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id, external_id
		)
		VALUES ($1, $9, $2, $3, $4, $5, 'approval-task', $6, $6, $7, $8)
	`, sessionID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID, externalID, streamTestPublicID(t, publicid.Session)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO streams (
			id, public_id, org_id, cell_id, project_id, environment_id, session_id,
			deployment_stream_id, name, direction, schema_fingerprint
		)
		VALUES ($1, $11, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, streamID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, sessionID, deploymentStreamID, streamName, direction, "schema-"+streamName, streamTestPublicID(t, publicid.Stream)); err != nil {
		t.Fatal(err)
	}
	return sessionID, streamID
}

func seedControlDeploymentStream(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids streamTokenRouteFixture, deploymentID uuid.UUID, name string, direction string, schemaFingerprint string, schemaJSON string) uuid.UUID {
	t.Helper()
	deploymentStreamID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO deployment_streams (id, org_id, cell_id, project_id, environment_id, deployment_id, name, direction, schema_fingerprint, schema_json) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, deploymentStreamID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, deploymentID, name, direction, schemaFingerprint, schemaJSON); err != nil {
		t.Fatal(err)
	}
	return deploymentStreamID
}

func testProjectIDStringUUID() uuid.UUID {
	return uuid.MustParse(testProjectIDString())
}

func testEnvironmentIDStringUUID() uuid.UUID {
	return uuid.MustParse(testEnvironmentIDString())
}
