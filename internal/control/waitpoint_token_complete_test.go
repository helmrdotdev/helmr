package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicaccess"
	"github.com/helmrdotdev/helmr/internal/waitpoint"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const waitpointTokenTestAuthSecret = "abcdefghijabcdefghijabcdefghij12"

func TestExtractInlineWaitpointTokenRejectsPublicAccessToken(t *testing.T) {
	tokenID := uuid.Must(uuid.NewV7())
	_, _, err := extractInlineWaitpointToken([]byte(`{"token_id":` + strconv.Quote(tokenID.String()) + `,"public_access_token":"hlmr_pat_old"}`))
	if err == nil {
		t.Fatal("expected public_access_token to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}

func TestWaitpointTokenCompleteRouteRequiresTransactionalStorageForPublicBearer(t *testing.T) {
	publicToken, publicTokenHash, err := publicaccess.NewToken([]byte(waitpointTokenTestAuthSecret))
	if err != nil {
		t.Fatal(err)
	}
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000401")
	publicAccessTokenID := uuid.MustParse("00000000-0000-0000-0000-000000000411")
	store := &fakeStore{
		waitpointToken:    testWaitpointToken(tokenID, publicTokenHash),
		publicAccessToken: testPublicAccessToken(publicAccessTokenID, publicTokenHash, tokenID),
	}
	handler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:         store,
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":true}}`))
	req.Header.Set("authorization", "Bearer "+publicToken)
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.lockPublicAccessTokenByHash) > 0 {
		t.Fatalf("public token should not be locked without transactional storage: %x", store.lockPublicAccessTokenByHash)
	}
	if store.consumePublicAccessToken.ID.Valid {
		t.Fatalf("public token should not be consumed without transactional storage: %+v", store.consumePublicAccessToken)
	}
	if store.completeWaitpointToken.ID.Valid {
		t.Fatalf("completion should not run without transactional storage: %+v", store.completeWaitpointToken)
	}
}

func TestWaitpointTokenCompleteRouteRejectsCallbackSecretBearer(t *testing.T) {
	callbackSecret, callbackSecretHash, err := waitpoint.NewCallbackSecret([]byte(waitpointTokenTestAuthSecret))
	if err != nil {
		t.Fatal(err)
	}
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000410")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	store.waitpointToken.CallbackSecretHash = callbackSecretHash
	handler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:         store,
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":true}}`))
	req.Header.Set("authorization", "Bearer "+callbackSecret)
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.getWaitpointTokenForAuthenticatedCompletion.ID.Valid || len(store.lockPublicAccessTokenByHash) > 0 {
		t.Fatalf("callback bearer should not look up token: auth=%+v public=%x", store.getWaitpointTokenForAuthenticatedCompletion, store.lockPublicAccessTokenByHash)
	}
	if store.completeWaitpointToken.ID.Valid {
		t.Fatalf("completion should not run for callback bearer: %+v", store.completeWaitpointToken)
	}
}

func TestPublicAccessTokenAllowsWaitpointCompletionRequiresTokenBinding(t *testing.T) {
	_, publicTokenHash, err := publicaccess.NewToken([]byte(waitpointTokenTestAuthSecret))
	if err != nil {
		t.Fatal(err)
	}
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000413")
	otherTokenID := uuid.MustParse("00000000-0000-0000-0000-000000000414")
	token := testPublicAccessToken(uuid.MustParse("00000000-0000-0000-0000-000000000415"), publicTokenHash, tokenID)
	if !publicAccessTokenAllowsWaitpointCompletion(token.AllowedScopes, tokenID) {
		t.Fatal("matching waitpoint token scope was rejected")
	}
	if publicAccessTokenAllowsWaitpointCompletion(token.AllowedScopes, otherTokenID) {
		t.Fatal("mismatched waitpoint token scope was accepted")
	}
}

func TestWaitpointTokenCallbackRouteAcceptsCallbackSecret(t *testing.T) {
	callbackSecret, callbackSecretHash, err := waitpoint.NewCallbackSecret([]byte(waitpointTokenTestAuthSecret))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(callbackSecret, "hlmr_pat_") {
		t.Fatalf("callback secret used public access token prefix: %s", callbackSecret)
	}
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000407")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	store.waitpointToken.CallbackSecretHash = callbackSecretHash
	handler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:         store,
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/callback/"+callbackSecret, strings.NewReader(`{"approved":true}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.getWaitpointTokenForCallbackCompletion.ID != pgvalue.UUID(tokenID) {
		t.Fatalf("callback lookup = %+v", store.getWaitpointTokenForCallbackCompletion)
	}
	if !bytes.Equal(store.getWaitpointTokenForCallbackCompletion.CallbackSecretHash, callbackSecretHash) {
		t.Fatal("callback lookup did not use the waitpoint callback secret hash")
	}
	if got := string(store.completeWaitpointToken.Data); got != `{"approved":true}` {
		t.Fatalf("completion data = %s", got)
	}
}

func TestWaitpointTokenCompleteRouteAcceptsAPIKeyBearer(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000402")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	handler := newTestServer(testServerConfig{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:  store,
		Auth: fakeAuth{
			projectID:     testProjectIDString(),
			environmentID: testEnvironmentIDString(),
			permissions:   []auth.Permission{auth.PermissionWaitpointTokensComplete},
		},
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":false}}`))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.getWaitpointTokenForAuthenticatedCompletion.ID != pgvalue.UUID(tokenID) {
		t.Fatalf("authenticated lookup = %+v", store.getWaitpointTokenForAuthenticatedCompletion)
	}
	if store.getWaitpointTokenForAuthenticatedCompletion.OrgID != pgvalue.UUID(dbtest.DefaultOrgID) {
		t.Fatalf("authenticated org scope = %+v", store.getWaitpointTokenForAuthenticatedCompletion)
	}
	if len(store.lockPublicAccessTokenByHash) > 0 {
		t.Fatalf("public lookup should not run for API key token: %x", store.lockPublicAccessTokenByHash)
	}
	if got := string(store.completeWaitpointToken.Data); got != `{"approved":false}` {
		t.Fatalf("completion data = %s", got)
	}
}

func TestWaitpointTokenCompleteRouteRejectsMetadataField(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000412")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	handler := newTestServer(testServerConfig{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:  store,
		Auth: fakeAuth{
			projectID:     testProjectIDString(),
			environmentID: testEnvironmentIDString(),
			permissions:   []auth.Permission{auth.PermissionWaitpointTokensComplete},
		},
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":true},"metadata":{"actor":"alice"}}`))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.completeWaitpointToken.ID.Valid {
		t.Fatalf("completion should not run with metadata field: %+v", store.completeWaitpointToken)
	}
}

func TestWaitpointTokenCompleteRouteAcceptsSessionBearer(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000408")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	handler := newTestServer(testServerConfig{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:  store,
		Auth: fakeAuth{
			kind: auth.ActorKindSession,
			role: auth.RoleDeveloper,
		},
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":true}}`))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"session-token")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.getWaitpointTokenForAuthenticatedCompletion.ID != pgvalue.UUID(tokenID) {
		t.Fatalf("authenticated lookup = %+v", store.getWaitpointTokenForAuthenticatedCompletion)
	}
	if got := string(store.completeWaitpointToken.Data); got != `{"approved":true}` {
		t.Fatalf("completion data = %s", got)
	}
}

func TestWaitpointTokenCompleteRouteRejectsCrossOrgAuthenticatedToken(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000409")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	store.waitpointToken.OrgID = pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-00000000f00d"))
	handler := newTestServer(testServerConfig{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:  store,
		Auth: fakeAuth{
			projectID:     testProjectIDString(),
			environmentID: testEnvironmentIDString(),
			permissions:   []auth.Permission{auth.PermissionWaitpointTokensComplete},
		},
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":true}}`))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.completeWaitpointToken.ID.Valid {
		t.Fatalf("completion should not run across orgs: %+v", store.completeWaitpointToken)
	}
}

func TestWaitpointTokenCompleteRouteRejectsDifferentEnvironmentAPIKey(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000411")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	store.waitpointToken.ProjectID = otherProjectID()
	store.waitpointToken.EnvironmentID = otherEnvironmentID()
	handler := newTestServer(testServerConfig{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:  store,
		Auth: fakeAuth{
			projectID:     testProjectIDString(),
			environmentID: testEnvironmentIDString(),
			permissions:   []auth.Permission{auth.PermissionWaitpointTokensComplete},
		},
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":true}}`))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.getWaitpointTokenForAuthenticatedCompletion.ID != pgvalue.UUID(tokenID) {
		t.Fatalf("authenticated lookup = %+v", store.getWaitpointTokenForAuthenticatedCompletion)
	}
	if store.completeWaitpointToken.ID.Valid {
		t.Fatalf("completion should not run for a different environment: %+v", store.completeWaitpointToken)
	}
}

func TestWaitpointTokenCompleteRouteRejectsAPIKeyWithoutEnvironmentScope(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000406")
	store := &fakeStore{waitpointToken: testWaitpointToken(tokenID, []byte("public-hash"))}
	handler := newTestServer(testServerConfig{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:  store,
		Auth: fakeAuth{
			kind:        auth.ActorKindAPIKey,
			permissions: []auth.Permission{auth.PermissionWaitpointTokensComplete},
		},
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":false}}`))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.getWaitpointTokenForAuthenticatedCompletion.ID != pgvalue.UUID(tokenID) {
		t.Fatalf("authenticated lookup = %+v", store.getWaitpointTokenForAuthenticatedCompletion)
	}
	if store.completeWaitpointToken.ID.Valid {
		t.Fatalf("completion should not run for unscoped API key: %+v", store.completeWaitpointToken)
	}
}

func TestWaitpointCompletionHashUsesData(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000499")
	data := json.RawMessage(`{"approved":true}`)
	internalScope := waitpointCompletionHashInternalScope()
	hash := mustWaitpointCompletionHash(t, tokenID, data, internalScope)
	if hash == mustWaitpointCompletionHash(t, tokenID, json.RawMessage(`{"approved":false}`), internalScope) {
		t.Fatal("completion hash should change when data changes")
	}
	if hash != mustWaitpointCompletionHash(t, tokenID, data, internalScope) {
		t.Fatal("completion hash should be stable for the same data")
	}
	if hash != mustWaitpointCompletionHash(t, tokenID, json.RawMessage(`{
		"approved": true
	}`), internalScope) {
		t.Fatal("completion hash should ignore JSON whitespace")
	}
	if mustWaitpointCompletionHash(t, tokenID, json.RawMessage(`{"reason":"ok","approved":true}`), internalScope) != mustWaitpointCompletionHash(t, tokenID, json.RawMessage(`{"approved":true,"reason":"ok"}`), internalScope) {
		t.Fatal("completion hash should ignore JSON object key order")
	}
}

func TestWaitpointCompletionHashUsesAuthScope(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000498")
	data := json.RawMessage(`{"approved":true}`)
	publicScope := waitpointCompletionHashPublicScope(publicAccessTokenScope{
		Type:             "waitpointToken.complete",
		WaitpointTokenID: tokenID.String(),
	})
	otherPublicScope := waitpointCompletionHashPublicScope(publicAccessTokenScope{
		Type:             "waitpointToken.complete",
		WaitpointTokenID: tokenID.String(),
		CorrelationID:    "other",
	})
	if mustWaitpointCompletionHash(t, tokenID, data, waitpointCompletionHashInternalScope()) == mustWaitpointCompletionHash(t, tokenID, data, publicScope) {
		t.Fatal("public token completion hash should differ from internal completion hash")
	}
	if mustWaitpointCompletionHash(t, tokenID, data, publicScope) == mustWaitpointCompletionHash(t, tokenID, data, otherPublicScope) {
		t.Fatal("public token completion hash should include the matched scope")
	}
}

func mustWaitpointCompletionHash(t *testing.T, tokenID uuid.UUID, data json.RawMessage, authScope json.RawMessage) string {
	t.Helper()
	hash, err := waitpointCompletionHash(tokenID, data, authScope)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestWaitpointTokenCompleteRouteRejectsMissingBearer(t *testing.T) {
	tokenID := uuid.MustParse("00000000-0000-0000-0000-000000000405")
	handler := newTestServer(testServerConfig{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:         &fakeStore{},
		AuthSecret: []byte(waitpointTokenTestAuthSecret),
		PublicURL:  mustParseTestURL("https://helmr.example.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"data":{"approved":true}}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func testWaitpointToken(id uuid.UUID, _ []byte) db.WaitpointToken {
	return db.WaitpointToken{
		ID:            pgvalue.UUID(id),
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		Status:        db.WaitpointTokenStatusWaiting,
		TimeoutAt:     pgvalue.Timestamptz(testTime().Time.Add(time.Hour)),
		CreatedAt:     testTime(),
		UpdatedAt:     testTime(),
	}
}

func testPublicAccessToken(id uuid.UUID, tokenHash []byte, waitpointTokenID uuid.UUID) db.PublicAccessToken {
	scopes, err := json.Marshal([]publicAccessTokenScope{{
		Type:             "waitpointToken.complete",
		WaitpointTokenID: waitpointTokenID.String(),
	}})
	if err != nil {
		panic(err)
	}
	return db.PublicAccessToken{
		ID:            pgvalue.UUID(id),
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		TokenHash:     tokenHash,
		AllowedScopes: scopes,
		ExpiresAt:     pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		MaxUses:       pgtype.Int4{Int32: 1, Valid: true},
		CreatedAt:     testTime(),
	}
}

func (f *fakeStore) GetWaitpointToken(_ context.Context, arg db.GetWaitpointTokenParams) (db.WaitpointToken, error) {
	f.getWaitpointToken = arg
	if f.waitpointToken.ID != arg.ID ||
		f.waitpointToken.OrgID != arg.OrgID ||
		f.waitpointToken.ProjectID != arg.ProjectID ||
		f.waitpointToken.EnvironmentID != arg.EnvironmentID {
		return db.WaitpointToken{}, pgx.ErrNoRows
	}
	return f.waitpointToken, nil
}

func (f *fakeStore) GetWaitpointTokenForAuthenticatedCompletion(_ context.Context, arg db.GetWaitpointTokenForAuthenticatedCompletionParams) (db.WaitpointToken, error) {
	f.getWaitpointTokenForAuthenticatedCompletion = arg
	if f.waitpointToken.ID != arg.ID || f.waitpointToken.OrgID != arg.OrgID {
		return db.WaitpointToken{}, pgx.ErrNoRows
	}
	return f.waitpointToken, nil
}

func (f *fakeStore) GetWaitpointTokenForPublicCompletion(_ context.Context, arg db.GetWaitpointTokenForPublicCompletionParams) (db.WaitpointToken, error) {
	f.getWaitpointTokenForPublicCompletion = arg
	if f.waitpointToken.ID != arg.ID ||
		f.waitpointToken.OrgID != arg.OrgID ||
		f.waitpointToken.ProjectID != arg.ProjectID ||
		f.waitpointToken.EnvironmentID != arg.EnvironmentID {
		return db.WaitpointToken{}, pgx.ErrNoRows
	}
	return f.waitpointToken, nil
}

func (f *fakeStore) GetWaitpointTokenForCallbackCompletion(_ context.Context, arg db.GetWaitpointTokenForCallbackCompletionParams) (db.WaitpointToken, error) {
	f.getWaitpointTokenForCallbackCompletion = arg
	if f.waitpointToken.ID != arg.ID || !bytes.Equal(f.waitpointToken.CallbackSecretHash, arg.CallbackSecretHash) {
		return db.WaitpointToken{}, pgx.ErrNoRows
	}
	return f.waitpointToken, nil
}

func (f *fakeStore) LockPublicAccessTokenByHash(_ context.Context, tokenHash []byte) (db.PublicAccessToken, error) {
	f.lockPublicAccessTokenByHash = tokenHash
	if !bytes.Equal(f.publicAccessToken.TokenHash, tokenHash) {
		return db.PublicAccessToken{}, pgx.ErrNoRows
	}
	return f.publicAccessToken, nil
}

func (f *fakeStore) ConsumePublicAccessToken(_ context.Context, arg db.ConsumePublicAccessTokenParams) (db.PublicAccessToken, error) {
	f.consumePublicAccessToken = arg
	if f.publicAccessToken.ID != arg.ID || f.publicAccessToken.OrgID != arg.OrgID {
		return db.PublicAccessToken{}, pgx.ErrNoRows
	}
	f.publicAccessToken.UsedCount++
	return f.publicAccessToken, nil
}

func (f *fakeStore) CreatePublicAccessToken(_ context.Context, arg db.CreatePublicAccessTokenParams) (db.PublicAccessToken, error) {
	f.createPublicAccessToken = arg
	return db.PublicAccessToken{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		TokenHash:     arg.TokenHash,
		AllowedScopes: arg.AllowedScopes,
		Metadata:      arg.Metadata,
		CreatedBy:     arg.CreatedBy,
		ExpiresAt:     arg.ExpiresAt,
		MaxUses:       arg.MaxUses,
	}, nil
}

func (f *fakeStore) CompleteWaitpointToken(_ context.Context, arg db.CompleteWaitpointTokenParams) (db.CompleteWaitpointTokenRow, error) {
	f.completeWaitpointToken = arg
	if f.waitpointToken.ID != arg.ID || f.waitpointToken.OrgID != arg.OrgID {
		return db.CompleteWaitpointTokenRow{}, pgx.ErrNoRows
	}
	return db.CompleteWaitpointTokenRow{
		ID:            f.waitpointToken.ID,
		OrgID:         f.waitpointToken.OrgID,
		ProjectID:     f.waitpointToken.ProjectID,
		EnvironmentID: f.waitpointToken.EnvironmentID,
		Status:        db.WaitpointTokenStatusCompleted,
		Data:          arg.Data,
		TimeoutAt:     f.waitpointToken.TimeoutAt,
		CompletedAt:   testTime(),
		Tags:          f.waitpointToken.Tags,
		Metadata:      f.waitpointToken.Metadata,
		CreatedAt:     f.waitpointToken.CreatedAt,
		UpdatedAt:     testTime(),
	}, nil
}
