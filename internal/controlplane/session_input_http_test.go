package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDecodeActorInputRequestAcceptsNullInput(t *testing.T) {
	request := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"input":null}`,
	))
	decoded, err := decodeSessionInputRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Input) != "null" {
		t.Fatalf("input = %s, want null", decoded.Input)
	}
}

func TestDecodeActorInputRequestRejectsAmbiguousJSON(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"input":1,"input":2}`,
		`{"input":1,"unknown":true}`,
		`{"input":1} true`,
	} {
		request := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if _, err := decodeSessionInputRequest(request); err == nil {
			t.Fatalf("decodeSessionInputRequest(%s) succeeded, want error", body)
		}
	}
}

func TestActorInputAPIKeyScopeRoundTripsPermission(t *testing.T) {
	normalized, ok := normalizeAPIKeyScope(api.APIKeyScopeSessionsInputSend)
	if !ok || normalized != api.APIKeyScopeSessionsInputSend {
		t.Fatalf("normalized scope = %q, %t", normalized, ok)
	}
	permission, ok := apiKeyScopePermission(normalized)
	if !ok || permission != auth.PermissionSessionsInputSend {
		t.Fatalf("permission = %q, %t", permission, ok)
	}
	scope, ok := apiKeyPermissionScope(string(permission))
	if !ok || scope != api.APIKeyScopeSessionsInputSend {
		t.Fatalf("scope = %q, %t", scope, ok)
	}
}

func TestActorInputSequenceExhaustionIsDistinctFromRetryableCapacity(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/", nil)
	(&Server{}).writeSessionInputAppendError(
		recorder,
		request,
		db.Session{},
		errActorSequenceExhausted,
	)
	response := decodeHTTPError(t, recorder.Body.Bytes())
	if recorder.Code != 409 || response.Code != "session_sequence_exhausted" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestActorInputBodyLimitReturnsTypedErrorForFixedAndChunkedBodies(t *testing.T) {
	handler := limitSessionInputBody(http.HandlerFunc((&Server{}).sendSessionInput))
	for _, chunked := range []bool{false, true} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/inputs",
			strings.NewReader(strings.Repeat("x", int(sessionInputBodyLimit+1))),
		)
		if chunked {
			request.ContentLength = -1
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		response := decodeHTTPError(t, recorder.Body.Bytes())
		if recorder.Code != http.StatusRequestEntityTooLarge ||
			response.Code != "session_input_too_large" {
			t.Fatalf("chunked=%t status=%d body=%s", chunked, recorder.Code, recorder.Body.String())
		}
	}
}

func TestActorInputBodyLimitAllowsMaximumEscapedEnvelope(t *testing.T) {
	input := `"` + strings.Repeat("a", maxActorInputBytes-2) + `"`
	idempotencyKey := "a" + strings.Repeat("\x01", 510) + "a"
	body, err := json.Marshal(api.SendSessionInputRequest{
		Input:          json.RawMessage(input),
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) > sessionInputBodyLimit {
		t.Fatalf("encoded maximum envelope = %d bytes, limit = %d", len(body), sessionInputBodyLimit)
	}
	called := false
	handler := limitSessionInputBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if !called || recorder.Code != http.StatusNoContent {
		t.Fatalf("called=%t status=%d", called, recorder.Code)
	}
}

func TestSendActorInputEnforcesAPIKeyPermissionAndEnvironmentScope(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	otherEnvironmentID := uuid.NewV7()
	actorID := uuid.NewV7()
	body := `{"input":{"type":"continue"},"idempotency_key":"message:1"}`

	t.Run("missing permission", func(t *testing.T) {
		store := newActorInputClaimStore()
		server := &Server{db: store}
		request := actorInputHandlerRequest(t, body, auth.Actor{
			OrgID:         orgID,
			Kind:          auth.ActorKindAPIKey,
			Role:          auth.RoleDeveloper,
			ProjectID:     projectID.String(),
			EnvironmentID: environmentID.String(),
		}, "", "", actorID.String())
		recorder := httptest.NewRecorder()
		server.sendSessionInput(recorder, request)
		if recorder.Code != http.StatusForbidden || store.actorReads != 0 {
			t.Fatalf("status=%d actor reads=%d body=%s", recorder.Code, store.actorReads, recorder.Body.String())
		}
	})

	t.Run("different environment", func(t *testing.T) {
		store := newActorInputClaimStore()
		store.locator = db.Session{
			ID:              pgvalue.UUID(actorID),
			EnvironmentID:   pgvalue.UUID(otherEnvironmentID),
			ActorDeclaredID: "operator",
			Key:             pgvalue.Text("thread:1"),
		}
		server := &Server{db: store}
		request := actorInputHandlerRequest(t, body, auth.Actor{
			OrgID:         orgID,
			Kind:          auth.ActorKindAPIKey,
			Role:          auth.RoleDeveloper,
			ProjectID:     projectID.String(),
			EnvironmentID: environmentID.String(),
			Permissions:   []auth.Permission{auth.PermissionSessionsInputSend},
		}, "", "", actorID.String())
		recorder := httptest.NewRecorder()
		server.sendSessionInput(recorder, request)
		if recorder.Code != http.StatusNotFound || store.actorReads != 1 {
			t.Fatalf("status=%d actor reads=%d body=%s", recorder.Code, store.actorReads, recorder.Body.String())
		}
	})

	t.Run("allowed replay", func(t *testing.T) {
		store := newActorInputClaimStore()
		store.locator = db.Session{
			ID:              pgvalue.UUID(actorID),
			EnvironmentID:   pgvalue.UUID(environmentID),
			ActorDeclaredID: "operator",
			Key:             pgvalue.Text("thread:1"),
		}
		completeActorInputClaim(
			t,
			store,
			environmentID,
			actorID,
			"message:1",
			[]byte(`{"type":"continue"}`),
			uuid.NewV7(),
			9,
		)
		store.calls = nil
		server := &Server{db: store}
		request := actorInputHandlerRequest(t, body, auth.Actor{
			OrgID:         orgID,
			Kind:          auth.ActorKindAPIKey,
			Role:          auth.RoleDeveloper,
			ProjectID:     projectID.String(),
			EnvironmentID: environmentID.String(),
			Permissions:   []auth.Permission{auth.PermissionSessionsInputSend},
		}, "", "", actorID.String())
		recorder := httptest.NewRecorder()
		server.sendSessionInput(recorder, request)
		if recorder.Code != http.StatusCreated || store.actorReads != 1 ||
			!strings.Contains(recorder.Body.String(), `"sequence":9`) {
			t.Fatalf("status=%d actor reads=%d body=%s", recorder.Code, store.actorReads, recorder.Body.String())
		}
	})
}

func TestSendActorInputDeniesViewerSessionBeforeActorLookup(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	store := newActorInputClaimStore()
	store.project = db.Project{
		ID:    pgvalue.UUID(projectID),
		OrgID: pgvalue.UUID(orgID),
	}
	store.environment = db.Environment{
		ID:        pgvalue.UUID(environmentID),
		OrgID:     pgvalue.UUID(orgID),
		ProjectID: pgvalue.UUID(projectID),
	}
	server := &Server{db: store}
	request := actorInputHandlerRequest(
		t,
		`{"input":null}`,
		auth.Actor{OrgID: orgID, Kind: auth.ActorKindSession, Role: auth.RoleViewer},
		projectID.String(),
		environmentID.String(),
		uuid.NewV7().String(),
	)
	recorder := httptest.NewRecorder()
	server.sendSessionInput(recorder, request)
	if recorder.Code != http.StatusForbidden || store.addressReads != 0 {
		t.Fatalf("status=%d address reads=%d body=%s", recorder.Code, store.addressReads, recorder.Body.String())
	}
}

func actorInputHandlerRequest(
	t *testing.T,
	body string,
	principal auth.Actor,
	projectID string,
	environmentID string,
	sessionID string,
) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	route := chi.NewRouteContext()
	if projectID != "" {
		route.URLParams.Add("projectID", projectID)
	}
	if environmentID != "" {
		route.URLParams.Add("environmentID", environmentID)
	}
	route.URLParams.Add("sessionID", sessionID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
