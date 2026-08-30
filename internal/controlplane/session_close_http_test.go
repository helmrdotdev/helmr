package controlplane

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestActorCloseAPIKeyScopeRoundTripsPermission(t *testing.T) {
	normalized, ok := normalizeAPIKeyScope(api.APIKeyScopeSessionsClose)
	if !ok || normalized != api.APIKeyScopeSessionsClose {
		t.Fatalf("normalized scope = %q, %t", normalized, ok)
	}
	permission, ok := apiKeyScopePermission(normalized)
	if !ok || permission != auth.PermissionSessionsClose {
		t.Fatalf("permission = %q, %t", permission, ok)
	}
	scope, ok := apiKeyPermissionScope(string(permission))
	if !ok || scope != api.APIKeyScopeSessionsClose {
		t.Fatalf("scope = %q, %t", scope, ok)
	}
}

func TestDecodeActorCloseRequestIsClosedAndPresenceAware(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"idempotency_key":"close-1"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, err := decodeSessionCloseRequest(request); err != nil {
			t.Fatalf("decodeSessionCloseRequest(%s): %v", body, err)
		}
	}
	for _, body := range []string{
		`null`,
		`[]`,
		`{"idempotency_key":null}`,
		`{"idempotency_key":""}`,
		`{"unknown":true}`,
		`{"idempotency_key":"a","idempotency_key":"b"}`,
		`{} {}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, err := decodeSessionCloseRequest(request); err == nil {
			t.Fatalf("decodeSessionCloseRequest(%s) succeeded", body)
		}
	}
}

func TestAuthorizeActorCloseRequiresExactPermission(t *testing.T) {
	projectID := uuid.NewV7().String()
	environmentID := uuid.NewV7().String()
	apiKey := auth.Actor{
		Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: projectID, EnvironmentID: environmentID,
		Permissions: []auth.Permission{auth.PermissionSessionsClose},
	}
	if err := authorizeSessionCloseBeforeLookup(apiKey); err != nil {
		t.Fatal(err)
	}
	if err := authorizeSessionCloseBeforeLookup(auth.Actor{
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
		if err := authorizeSessionCloseBeforeLookup(principal); err == nil {
			t.Fatalf("principal %+v was authorized", principal)
		}
	}
}

func TestActorCloseBodyLimitReturnsTypedError(t *testing.T) {
	handler := limitSessionCloseBody(http.HandlerFunc((&Server{}).closeSessionHTTP))
	for _, chunked := range []bool{false, true} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/close",
			strings.NewReader(strings.Repeat("x", int(sessionCloseBodyLimit+1))),
		)
		if chunked {
			request.ContentLength = -1
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		response := decodeHTTPError(t, recorder.Body.Bytes())
		if recorder.Code != http.StatusRequestEntityTooLarge ||
			response.Code != "session_close_request_too_large" {
			t.Fatalf("chunked=%t status=%d body=%s", chunked, recorder.Code, recorder.Body.String())
		}
	}
}

func TestWriteActorCloseErrorUsesStableCodes(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{err: errActorCloseConflict, status: http.StatusConflict, code: "session_close_conflict"},
		{
			err: errActorCloseAuthority, status: http.StatusServiceUnavailable,
			code: "session_close_authority_unavailable",
		},
	} {
		recorder := httptest.NewRecorder()
		(&Server{}).writeSessionCloseError(recorder, test.err)
		response := decodeHTTPError(t, recorder.Body.Bytes())
		if recorder.Code != test.status || response.Code != test.code {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestWriteActorCloseScopeErrorDistinguishesReferencesFromAuthority(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{
			err:    invalidEnvironmentScopeReference("project_id is invalid"),
			status: http.StatusBadRequest,
			code:   "invalid_session_close",
		},
		{
			err:    errors.New("database unavailable"),
			status: http.StatusServiceUnavailable,
			code:   "session_close_authority_unavailable",
		},
	} {
		recorder := httptest.NewRecorder()
		(&Server{}).writeSessionCloseScopeError(recorder, test.err)
		response := decodeHTTPError(t, recorder.Body.Bytes())
		if recorder.Code != test.status || response.Code != test.code {
			t.Fatalf("error=%v status=%d body=%s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}
