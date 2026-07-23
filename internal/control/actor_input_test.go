package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDecodeActorInputRequestAcceptsNullInput(t *testing.T) {
	request := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa","input":null}`,
	))
	decoded, err := decodeActorInputRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Input) != "null" {
		t.Fatalf("input = %s, want null", decoded.Input)
	}
}

func TestDecodeActorInputRequestRejectsAmbiguousJSON(t *testing.T) {
	for _, body := range []string{
		`{"actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa","input":1,"input":2}`,
		`{"actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa","input":1,"unknown":true}`,
		`{"actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa","input":1} true`,
	} {
		request := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if _, err := decodeActorInputRequest(request); err == nil {
			t.Fatalf("decodeActorInputRequest(%s) succeeded, want error", body)
		}
	}
}

func TestValidateActorInputAddressRequiresExclusiveAddress(t *testing.T) {
	for _, request := range []api.SendActorInputRequest{
		{},
		{ActorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa", ActorKey: "thread:1"},
	} {
		if err := validateActorInputAddress(request); err == nil {
			t.Fatalf("validateActorInputAddress(%+v) succeeded, want error", request)
		}
	}
}

func TestActorInputAPIKeyScopeRoundTripsPermission(t *testing.T) {
	normalized, ok := normalizeAPIKeyScope(api.APIKeyScopeActorsInputSend)
	if !ok || normalized != api.APIKeyScopeActorsInputSend {
		t.Fatalf("normalized scope = %q, %t", normalized, ok)
	}
	permission, ok := apiKeyScopePermission(normalized)
	if !ok || permission != auth.PermissionActorsInputSend {
		t.Fatalf("permission = %q, %t", permission, ok)
	}
	scope, ok := apiKeyPermissionScope(string(permission))
	if !ok || scope != api.APIKeyScopeActorsInputSend {
		t.Fatalf("scope = %q, %t", scope, ok)
	}
}

func TestActorInputSequenceExhaustionIsDistinctFromRetryableCapacity(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/", nil)
	(&Server{}).writeActorInputAppendError(
		recorder,
		request,
		db.Actor{},
		errActorSequenceExhausted,
	)
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 409 || response.Code != "actor_sequence_exhausted" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestActorInputBodyLimitReturnsTypedErrorForFixedAndChunkedBodies(t *testing.T) {
	handler := limitActorInputBody(http.HandlerFunc((&Server{}).sendActorInput))
	for _, chunked := range []bool{false, true} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/actors/operator/input",
			strings.NewReader(strings.Repeat("x", int(actorInputSendBodyLimit+1))),
		)
		if chunked {
			request.ContentLength = -1
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var response struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusRequestEntityTooLarge ||
			response.Code != "actor_input_too_large" {
			t.Fatalf("chunked=%t status=%d body=%s", chunked, recorder.Code, recorder.Body.String())
		}
	}
}

func TestActorInputBodyLimitAllowsMaximumEscapedEnvelope(t *testing.T) {
	input := `"` + strings.Repeat("a", maxActorInputBytes-2) + `"`
	actorKey := "a" + strings.Repeat("\x01", 510) + "a"
	idempotencyKey := "a" + strings.Repeat("\x01", 510) + "a"
	body, err := json.Marshal(api.SendActorInputRequest{
		ActorKey:       actorKey,
		Input:          json.RawMessage(input),
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) > actorInputSendBodyLimit {
		t.Fatalf("encoded maximum envelope = %d bytes, limit = %d", len(body), actorInputSendBodyLimit)
	}
	called := false
	handler := limitActorInputBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	orgID := uuid.New()
	projectID := uuid.New()
	environmentID := uuid.New()
	otherEnvironmentID := uuid.New()
	actorID := uuid.New()
	body := `{"actor_key":"thread:1","input":{"type":"continue"},"idempotency_key":"message:1"}`

	t.Run("missing permission", func(t *testing.T) {
		store, manager := newActorInputClaimStore(t)
		server := &Server{db: store, claims: manager}
		request := actorInputHandlerRequest(t, body, auth.Actor{
			OrgID:         orgID,
			Kind:          auth.ActorKindAPIKey,
			Role:          auth.RoleDeveloper,
			ProjectID:     projectID.String(),
			EnvironmentID: environmentID.String(),
		}, "", "", "operator")
		recorder := httptest.NewRecorder()
		server.sendActorInput(recorder, request)
		if recorder.Code != http.StatusForbidden || store.addressReads != 0 {
			t.Fatalf("status=%d address reads=%d body=%s", recorder.Code, store.addressReads, recorder.Body.String())
		}
	})

	t.Run("different environment", func(t *testing.T) {
		store, manager := newActorInputClaimStore(t)
		store.addressed = db.Actor{
			ID:              pgvalue.UUID(actorID),
			EnvironmentID:   pgvalue.UUID(otherEnvironmentID),
			ActorDeclaredID: "operator",
			Key:             pgvalue.Text("thread:1"),
		}
		server := &Server{db: store, claims: manager}
		request := actorInputHandlerRequest(t, body, auth.Actor{
			OrgID:         orgID,
			Kind:          auth.ActorKindAPIKey,
			Role:          auth.RoleDeveloper,
			ProjectID:     projectID.String(),
			EnvironmentID: environmentID.String(),
			Permissions:   []auth.Permission{auth.PermissionActorsInputSend},
		}, "", "", "operator")
		recorder := httptest.NewRecorder()
		server.sendActorInput(recorder, request)
		if recorder.Code != http.StatusNotFound || store.addressReads != 1 {
			t.Fatalf("status=%d address reads=%d body=%s", recorder.Code, store.addressReads, recorder.Body.String())
		}
	})

	t.Run("allowed replay", func(t *testing.T) {
		store, manager := newActorInputClaimStore(t)
		store.addressed = db.Actor{
			ID:              pgvalue.UUID(actorID),
			EnvironmentID:   pgvalue.UUID(environmentID),
			ActorDeclaredID: "operator",
			Key:             pgvalue.Text("thread:1"),
		}
		completeActorInputClaim(
			t,
			store,
			manager,
			environmentID,
			actorID,
			"message:1",
			[]byte(`{"type":"continue"}`),
			uuid.New(),
			9,
		)
		store.calls = nil
		server := &Server{db: store, claims: manager}
		request := actorInputHandlerRequest(t, body, auth.Actor{
			OrgID:         orgID,
			Kind:          auth.ActorKindAPIKey,
			Role:          auth.RoleDeveloper,
			ProjectID:     projectID.String(),
			EnvironmentID: environmentID.String(),
			Permissions:   []auth.Permission{auth.PermissionActorsInputSend},
		}, "", "", "operator")
		recorder := httptest.NewRecorder()
		server.sendActorInput(recorder, request)
		if recorder.Code != http.StatusOK || store.addressReads != 1 ||
			!strings.Contains(recorder.Body.String(), `"sequence":9`) {
			t.Fatalf("status=%d address reads=%d body=%s", recorder.Code, store.addressReads, recorder.Body.String())
		}
	})
}

func TestSendActorInputDeniesViewerSessionBeforeActorLookup(t *testing.T) {
	orgID := uuid.New()
	projectID := uuid.New()
	environmentID := uuid.New()
	store, manager := newActorInputClaimStore(t)
	store.project = db.Project{
		ID:    pgvalue.UUID(projectID),
		OrgID: pgvalue.UUID(orgID),
	}
	store.environment = db.Environment{
		ID:        pgvalue.UUID(environmentID),
		OrgID:     pgvalue.UUID(orgID),
		ProjectID: pgvalue.UUID(projectID),
	}
	server := &Server{db: store, claims: manager}
	request := actorInputHandlerRequest(
		t,
		`{"actor_key":"thread:1","input":null}`,
		auth.Actor{OrgID: orgID, Kind: auth.ActorKindSession, Role: auth.RoleViewer},
		projectID.String(),
		environmentID.String(),
		"operator",
	)
	recorder := httptest.NewRecorder()
	server.sendActorInput(recorder, request)
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
	actorDeclaredID string,
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
	route.URLParams.Add("actorDeclaredID", actorDeclaredID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
