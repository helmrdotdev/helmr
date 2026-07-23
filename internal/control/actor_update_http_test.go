package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestActorUpdateAPIKeyScopeRoundTripsPermission(t *testing.T) {
	normalized, ok := normalizeAPIKeyScope(api.APIKeyScopeActorsUpdate)
	if !ok || normalized != api.APIKeyScopeActorsUpdate {
		t.Fatalf("normalized scope = %q, %t", normalized, ok)
	}
	permission, ok := apiKeyScopePermission(normalized)
	if !ok || permission != auth.PermissionActorsUpdate {
		t.Fatalf("permission = %q, %t", permission, ok)
	}
	scope, ok := apiKeyPermissionScope(string(permission))
	if !ok || scope != api.APIKeyScopeActorsUpdate {
		t.Fatalf("scope = %q, %t", scope, ok)
	}
}

func TestAuthorizeActorUpdateAllowsWritersAndDeniesViewer(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7()).String()
	environmentID := uuid.Must(uuid.NewV7()).String()
	for _, principal := range []auth.Actor{
		{
			Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
			ProjectID: projectID, EnvironmentID: environmentID,
			Permissions: []auth.Permission{auth.PermissionActorsUpdate},
		},
		{Kind: auth.ActorKindSession, Role: auth.RoleOwner},
		{Kind: auth.ActorKindSession, Role: auth.RoleAdmin},
		{Kind: auth.ActorKindSession, Role: auth.RoleDeveloper},
	} {
		if err := authorizeActorUpdateBeforeLookup(principal); err != nil {
			t.Fatalf("principal %+v: %v", principal, err)
		}
	}
	for _, principal := range []auth.Actor{
		{Kind: auth.ActorKindSession, Role: auth.RoleViewer},
		{
			Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
			ProjectID: projectID, EnvironmentID: environmentID,
		},
		{
			Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
			Permissions: []auth.Permission{auth.PermissionActorsUpdate},
		},
	} {
		if err := authorizeActorUpdateBeforeLookup(principal); err == nil {
			t.Fatalf("principal %+v was authorized", principal)
		}
	}
}

func TestDecodeActorUpdateRequestUsesClosedPresenceSensitiveGrammar(t *testing.T) {
	validID := "act_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, body := range []string{
		`{"actor_id":"` + validID + `","metadata":{}}`,
		`{"actor_key":"thread:1","tags":[]}`,
		`{"actor_key":"thread:1","expires_at":"2030-01-02T03:04:05.123456789Z"}`,
		`{"actor_key":"thread:1","metadata":{"a":1},"tags":["alpha"],"expires_at":"2030-01-02T03:04:05+09:00"}`,
	} {
		request := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
		got, err := decodeUpdateActorRequest(request)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got.ActorID == "" && got.ActorKey == "" {
			t.Fatalf("%s decoded without address", body)
		}
	}

	for _, body := range []string{
		`null`,
		`[]`,
		`{}`,
		`{"actor_key":"thread:1"}`,
		`{"actor_id":null,"metadata":{}}`,
		`{"actor_key":"thread:1","metadata":null}`,
		`{"actor_key":"thread:1","tags":null}`,
		`{"actor_key":"thread:1","tags":[null]}`,
		`{"actor_key":"thread:1","expires_at":null}`,
		`{"actor_key":"thread:1","expires_at":"2030-01-02"}`,
		`{"actor_key":"thread:1","metadata":{},"unknown":true}`,
		`{"actor_key":"thread:1","metadata":{},"metadata":{"later":true}}`,
		`{"actor_key":"thread:1","metadata":{}} {}`,
		`{"actor_key":"thread:1","metadata":{"s":"\ud800"}}`,
	} {
		request := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
		if _, err := decodeUpdateActorRequest(request); err == nil {
			t.Fatalf("%s decoded successfully", body)
		}
	}
}

func TestActorUpdateDeniesBeforeScopeLookup(t *testing.T) {
	body := `{"actor_key":"thread:1","metadata":{}}`
	request := actorUpdateHTTPTestRequest(body, auth.Actor{
		Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID:     uuid.Must(uuid.NewV7()).String(),
		EnvironmentID: uuid.Must(uuid.NewV7()).String(),
	})
	recorder := httptest.NewRecorder()
	(&Server{}).updateActorHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden ||
		!strings.Contains(recorder.Body.String(), `"code":"permission_required"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestActorUpdateAuthenticationErrorsUseMachineReadableEnvelope(t *testing.T) {
	server := &Server{log: slog.Default()}
	for _, middleware := range []func(http.Handler) http.Handler{
		func(next http.Handler) http.Handler {
			return server.requireActorWithErrorWriter(next, writeActorUpdateAuthError)
		},
		func(next http.Handler) http.Handler {
			return server.requireSessionWithErrorWriter(next, writeActorUpdateAuthError)
		},
	} {
		request := httptest.NewRequest(http.MethodPatch, "/", nil)
		recorder := httptest.NewRecorder()
		middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("unauthenticated request reached handler")
		})).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized ||
			!strings.Contains(recorder.Body.String(), `"code":"authentication_required"`) {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestWriteActorUpdateErrorUsesStableCodes(t *testing.T) {
	server := &Server{}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{errActorUpdateInvalid, http.StatusBadRequest, "invalid_actor_update"},
		{errActorUpdateNotFound, http.StatusNotFound, "actor_not_found"},
		{errActorUpdateConflict, http.StatusConflict, "actor_update_conflict"},
		{errActorUpdateAuthority, http.StatusServiceUnavailable, "actor_update_authority_unavailable"},
	} {
		recorder := httptest.NewRecorder()
		server.writeActorUpdateError(recorder, test.err)
		var response struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != test.status || response.Code != test.code {
			t.Fatalf("%v: status=%d body=%s", test.err, recorder.Code, recorder.Body.String())
		}
		if test.status == http.StatusServiceUnavailable && !response.Retryable {
			t.Fatalf("%v should be retryable: %s", test.err, recorder.Body.String())
		}
	}
}

func TestActorUpdateBodyLimitUsesContractLimit(t *testing.T) {
	if actorUpdateBodyLimit != 81_920 {
		t.Fatalf("body limit = %d", actorUpdateBodyLimit)
	}
	handler := limitActorUpdateBody(http.HandlerFunc((&Server{}).updateActorHTTP))
	body := strings.Repeat("x", int(actorUpdateBodyLimit)+1)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body)))
	if recorder.Code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(recorder.Body.String(), `"code":"actor_update_request_too_large"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func actorUpdateHTTPTestRequest(body string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("actorDeclaredID", "operator.v1")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
