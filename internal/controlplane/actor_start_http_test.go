package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/idempotency"
)

func TestActorStartAPIKeyScopeRoundTrips(t *testing.T) {
	scope, ok := normalizeAPIKeyScope(api.APIKeyScopeActorsStart)
	if !ok || scope != api.APIKeyScopeActorsStart {
		t.Fatalf("normalized scope = %q, %t", scope, ok)
	}
	permission, ok := apiKeyScopePermission(scope)
	if !ok || permission != auth.PermissionActorsStart {
		t.Fatalf("permission = %q, %t", permission, ok)
	}
	roundTrip, ok := apiKeyPermissionScope(string(permission))
	if !ok || roundTrip != api.APIKeyScopeActorsStart {
		t.Fatalf("round-trip scope = %q, %t", roundTrip, ok)
	}
}

func TestDecodeStartActorRequestIsClosedAndPresenceAware(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"input":null}`),
	)
	decoded, err := decodeStartActorRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Input) == 0 || string(decoded.Input) != "null" {
		t.Fatalf("input = %s", decoded.Input)
	}

	for _, body := range []string{
		`{"workspace":{"key":"workspace:1"}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","unknown":true}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"unknown":true}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"backoff":{"unknown":true}}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}} {}`,
		`null`,
		`{"workspace":null}`,
		`{"workspace":{"key":null}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"key":null}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":null}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":null}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"queue":null}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"concurrency_key":null}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"priority":null}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"ttl":null}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":null}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"metadata":null}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"tags":[null]}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"enabled":null}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"max_attempts":null}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"backoff":null}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"backoff":{"min_delay":null}}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"backoff":{"max_delay":null}}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"backoff":{"factor":null}}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"backoff":{"jitter":null}}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":""}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"queue":""}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"ttl":""}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"max_attempts":3,"backoff":{"min_delay":""}}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"max_attempts":3,"backoff":{"max_delay":""}}}}`,
		`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"run":{"retry":{"max_attempts":3,"backoff":{"jitter":""}}}}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, err := decodeStartActorRequest(request); err == nil {
			t.Fatalf("decodeStartActorRequest(%s) succeeded", body)
		}
	}
}

func TestWriteActorStartErrorUsesStableCodes(t *testing.T) {
	server := &Server{}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{
			err:    idempotency.ConflictError{},
			status: http.StatusConflict,
			code:   "idempotency_conflict",
		},
		{
			err:    ActorKeyConflictError{Key: "thread:1"},
			status: http.StatusConflict,
			code:   "actor_key_conflict",
		},
		{err: errActorStartNotDeployed, status: http.StatusNotFound, code: "actor_not_deployed"},
		{err: errActorStartWorkspaceNotFound, status: http.StatusNotFound, code: "workspace_not_found"},
		{err: errActorStartWorkspaceConflict, status: http.StatusConflict, code: "workspace_unavailable"},
		{err: errActorStartSecretUnavailable, status: http.StatusConflict, code: "secret_unavailable"},
		{err: errActorInputTooLarge, status: http.StatusRequestEntityTooLarge, code: "actor_input_too_large"},
		{
			err:    errors.Join(errActorStartInvalid, errors.New("bad duration")),
			status: http.StatusBadRequest,
			code:   "invalid_actor_start",
		},
	} {
		recorder := httptest.NewRecorder()
		server.writeActorStartError(recorder, test.err)
		response := decodeHTTPError(t, recorder.Body.Bytes())
		if recorder.Code != test.status || response.Code != test.code {
			t.Fatalf("error=%v status=%d body=%s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}

func TestActorStartPresenceErrorsUseContractSpecificCodes(t *testing.T) {
	for _, test := range []struct {
		body string
		code string
	}{
		{body: `{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":null}`, code: "invalid_idempotency_key"},
		{body: `{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":""}`, code: "invalid_idempotency_key"},
		{body: `{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":" \t "}`, code: "invalid_idempotency_key"},
		{body: `{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":1}`, code: "invalid_idempotency_key"},
		{body: `{"workspace":null}`, code: "invalid_workspace_reference"},
		{body: `{"workspace":{"key":null}}`, code: "invalid_workspace_reference"},
		{body: `{"workspace":{"key":1}}`, code: "invalid_workspace_reference"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
		recorder := httptest.NewRecorder()
		(&Server{}).startActorHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest ||
			decodeHTTPError(t, recorder.Body.Bytes()).Code != test.code {
			t.Fatalf("body=%s status=%d response=%s", test.body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestWriteActorStartScopeErrorDistinguishesReferencesFromAuthority(t *testing.T) {
	server := &Server{}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{
			err:    invalidEnvironmentScopeReference("project_id is invalid"),
			status: http.StatusBadRequest,
			code:   "invalid_actor_start",
		},
		{
			err:    errors.New("database unavailable"),
			status: http.StatusServiceUnavailable,
			code:   "actor_start_authority_unavailable",
		},
	} {
		recorder := httptest.NewRecorder()
		server.writeActorStartScopeError(recorder, test.err)
		if recorder.Code != test.status ||
			decodeHTTPError(t, recorder.Body.Bytes()).Code != test.code {
			t.Fatalf("error=%v status=%d body=%s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAuthorizeActorStartRejectsBeforeScopeLookup(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/projects/missing/environments/missing/actors/operator.v1/start",
		strings.NewReader(`{"workspace":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}}`),
	)
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", "missing")
	route.URLParams.Add("environmentID", "missing")
	route.URLParams.Add("actorDeclaredID", "operator.v1")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, auth.Actor{
		Kind: auth.ActorKindSession,
		Role: auth.RoleViewer,
	}))

	recorder := httptest.NewRecorder()
	(&Server{}).startActorHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden ||
		!strings.Contains(recorder.Body.String(), `"code":"permission_required"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestActorStartAuthenticationErrorsUseMachineReadableEnvelope(t *testing.T) {
	server := &Server{log: slog.Default()}
	for _, middleware := range []func(http.Handler) http.Handler{
		func(next http.Handler) http.Handler {
			return server.requireActorWithErrorWriter(next, writeActorStartAuthError)
		},
		func(next http.Handler) http.Handler {
			return server.requireSessionWithErrorWriter(next, writeActorStartAuthError)
		},
	} {
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		recorder := httptest.NewRecorder()
		middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("unauthenticated request reached handler")
		})).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized ||
			decodeHTTPError(t, recorder.Body.Bytes()).Code != "authentication_required" {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestActorStartBodyLimitReturnsStableCode(t *testing.T) {
	server := &Server{}
	for _, chunked := range []bool{false, true} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/",
			strings.NewReader(strings.Repeat(" ", int(actorStartBodyLimit+1))),
		)
		if chunked {
			request.ContentLength = -1
		}
		recorder := httptest.NewRecorder()
		limitActorStartBody(http.HandlerFunc(server.startActorHTTP)).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusRequestEntityTooLarge ||
			!strings.Contains(recorder.Body.String(), `"code":"actor_start_request_too_large"`) {
			t.Fatalf("chunked=%t status=%d body=%s", chunked, recorder.Code, recorder.Body.String())
		}
	}
}

func TestActorStartRejectsOversizeCanonicalInput(t *testing.T) {
	workspaceID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	body := `{"workspace":{"id":"` + workspaceID + `"},"input":"` +
		strings.Repeat("a", maxActorInputBytes) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("actorDeclaredID", "operator.v1")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	recorder := httptest.NewRecorder()
	(&Server{}).startActorHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(recorder.Body.String(), `"code":"actor_input_too_large"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
