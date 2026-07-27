package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestActorCloseAPIKeyScopeRoundTripsPermission(t *testing.T) {
	normalized, ok := normalizeAPIKeyScope(api.APIKeyScopeActorsCloseManage)
	if !ok || normalized != api.APIKeyScopeActorsCloseManage {
		t.Fatalf("normalized scope = %q, %t", normalized, ok)
	}
	permission, ok := apiKeyScopePermission(normalized)
	if !ok || permission != auth.PermissionActorsCloseManage {
		t.Fatalf("permission = %q, %t", permission, ok)
	}
	scope, ok := apiKeyPermissionScope(string(permission))
	if !ok || scope != api.APIKeyScopeActorsCloseManage {
		t.Fatalf("scope = %q, %t", scope, ok)
	}
}

func TestDecodeActorCloseRequestIsClosedAndPresenceAware(t *testing.T) {
	for _, body := range []string{
		`{"actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"actor_key":"thread:1","idempotency_key":"close-1"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, err := decodeActorCloseRequest(request); err != nil {
			t.Fatalf("decodeActorCloseRequest(%s): %v", body, err)
		}
	}
	for _, body := range []string{
		`null`,
		`[]`,
		`{"actor_id":null}`,
		`{"actor_key":null}`,
		`{"actor_id":1}`,
		`{"actor_key":true}`,
		`{"actor_key":""}`,
		`{"actor_key":"thread:1","idempotency_key":null}`,
		`{"actor_key":"thread:1","idempotency_key":""}`,
		`{"actor_key":"thread:1","unknown":true}`,
		`{"actor_key":"thread:1","actor_key":"thread:2"}`,
		`{"actor_key":"thread:1"} {}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, err := decodeActorCloseRequest(request); err == nil {
			t.Fatalf("decodeActorCloseRequest(%s) succeeded", body)
		}
	}
}

func TestAuthorizeActorCloseRequiresExactPermission(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7()).String()
	environmentID := uuid.Must(uuid.NewV7()).String()
	apiKey := auth.Actor{
		Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: projectID, EnvironmentID: environmentID,
		Permissions: []auth.Permission{auth.PermissionActorsCloseManage},
	}
	if err := authorizeActorCloseBeforeLookup(apiKey); err != nil {
		t.Fatal(err)
	}
	if err := authorizeActorCloseBeforeLookup(auth.Actor{
		Kind: auth.ActorKindSession, Role: auth.RoleDeveloper,
	}); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []auth.Actor{
		{
			Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
			ProjectID: projectID, EnvironmentID: environmentID,
		},
		{Kind: auth.ActorKindSession, Role: auth.RoleViewer},
	} {
		if err := authorizeActorCloseBeforeLookup(principal); err == nil {
			t.Fatalf("principal %+v was authorized", principal)
		}
	}
}

func TestActorCloseBodyLimitReturnsTypedError(t *testing.T) {
	handler := limitActorCloseBody(http.HandlerFunc((&Server{}).closeActorHTTP))
	for _, chunked := range []bool{false, true} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/actors/operator.v1/close",
			strings.NewReader(strings.Repeat("x", int(actorCloseBodyLimit+1))),
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
			response.Code != "actor_close_request_too_large" {
			t.Fatalf("chunked=%t status=%d body=%s", chunked, recorder.Code, recorder.Body.String())
		}
	}
}

func TestWriteActorCloseErrorUsesStableCodes(t *testing.T) {
	for _, test := range []struct {
		err       error
		status    int
		code      string
		retryable bool
	}{
		{err: errActorCloseConflict, status: http.StatusConflict, code: "actor_close_conflict"},
		{
			err: errActorCloseAuthority, status: http.StatusServiceUnavailable,
			code: "actor_close_authority_unavailable", retryable: true,
		},
	} {
		recorder := httptest.NewRecorder()
		(&Server{}).writeActorCloseError(recorder, test.err)
		var response struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != test.status || response.Code != test.code ||
			response.Retryable != test.retryable {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestWriteActorCloseScopeErrorDistinguishesReferencesFromAuthority(t *testing.T) {
	for _, test := range []struct {
		err       error
		status    int
		code      string
		retryable bool
	}{
		{
			err:    invalidEnvironmentScopeReference("project_id is invalid"),
			status: http.StatusBadRequest,
			code:   "invalid_actor_close",
		},
		{
			err:       errors.New("database unavailable"),
			status:    http.StatusServiceUnavailable,
			code:      "actor_close_authority_unavailable",
			retryable: true,
		},
	} {
		recorder := httptest.NewRecorder()
		(&Server{}).writeActorCloseScopeError(recorder, test.err)
		var response struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != test.status || response.Code != test.code ||
			response.Retryable != test.retryable {
			t.Fatalf("error=%v status=%d body=%s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}
