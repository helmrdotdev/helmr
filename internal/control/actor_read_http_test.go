package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorReadAPIKeyScopeRoundTripsPermission(t *testing.T) {
	normalized, ok := normalizeAPIKeyScope(api.APIKeyScopeActorsRead)
	if !ok || normalized != api.APIKeyScopeActorsRead {
		t.Fatalf("normalized scope = %q, %t", normalized, ok)
	}
	permission, ok := apiKeyScopePermission(normalized)
	if !ok || permission != auth.PermissionActorsRead {
		t.Fatalf("permission = %q, %t", permission, ok)
	}
	scope, ok := apiKeyPermissionScope(string(permission))
	if !ok || scope != api.APIKeyScopeActorsRead {
		t.Fatalf("scope = %q, %t", scope, ok)
	}
}

func TestAuthorizeActorReadAcceptsReadOnlyRolesAndExactAPIKeyGrant(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7()).String()
	environmentID := uuid.Must(uuid.NewV7()).String()
	for _, principal := range []auth.Actor{
		{
			Kind:      auth.ActorKindAPIKey,
			Role:      auth.RoleDeveloper,
			ProjectID: projectID, EnvironmentID: environmentID,
			Permissions: []auth.Permission{auth.PermissionActorsRead},
		},
		{Kind: auth.ActorKindSession, Role: auth.RoleOwner},
		{Kind: auth.ActorKindSession, Role: auth.RoleAdmin},
		{Kind: auth.ActorKindSession, Role: auth.RoleDeveloper},
		{Kind: auth.ActorKindSession, Role: auth.RoleViewer},
	} {
		if err := authorizeActorReadBeforeLookup(principal); err != nil {
			t.Fatalf("principal %+v: %v", principal, err)
		}
	}
	for _, principal := range []auth.Actor{
		{
			Kind:      auth.ActorKindAPIKey,
			Role:      auth.RoleDeveloper,
			ProjectID: projectID, EnvironmentID: environmentID,
		},
		{
			Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
			Permissions: []auth.Permission{auth.PermissionActorsRead},
		},
	} {
		if err := authorizeActorReadBeforeLookup(principal); err == nil {
			t.Fatalf("principal %+v was authorized", principal)
		}
	}
}

func TestActorReadDeniesBeforeScopeLookup(t *testing.T) {
	for _, test := range []struct {
		handler http.HandlerFunc
		target  string
	}{
		{handler: (&Server{}).getActorStatusHTTP, target: "/?actor_key=thread%3A1"},
		{handler: (&Server{}).listActorsHTTP, target: "/"},
	} {
		request := httptest.NewRequest(http.MethodGet, test.target, nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("projectID", "missing")
		route.URLParams.Add("environmentID", "missing")
		route.URLParams.Add("actorDeclaredID", "operator.v1")
		ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
		ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{
			Kind:          auth.ActorKindAPIKey,
			ProjectID:     uuid.Must(uuid.NewV7()).String(),
			EnvironmentID: uuid.Must(uuid.NewV7()).String(),
		})
		request = request.WithContext(ctx)
		recorder := httptest.NewRecorder()
		test.handler.ServeHTTP(recorder, request)
		var response struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusForbidden || response.Code != "permission_required" {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestActorReadAuthenticationErrorsUseMachineReadableEnvelope(t *testing.T) {
	server := &Server{log: slog.Default()}
	for _, middleware := range []func(http.Handler) http.Handler{
		func(next http.Handler) http.Handler {
			return server.requireActorWithErrorWriter(next, writeActorReadAuthError)
		},
		func(next http.Handler) http.Handler {
			return server.requireSessionWithErrorWriter(next, writeActorReadAuthError)
		},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
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

func TestParseActorReadAddressRequiresOneClosedReference(t *testing.T) {
	validID := "act_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	for raw, want := range map[string]actorReadAddress{
		"?actor_id=" + validID:  {publicID: validID},
		"?actor_key=thread%3A1": {key: "thread:1"},
	} {
		request := httptest.NewRequest("GET", "/"+raw, nil)
		got, err := parseActorReadAddress(request)
		if err != nil {
			t.Fatalf("parseActorReadAddress(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("parseActorReadAddress(%q) = %+v, want %+v", raw, got, want)
		}
	}
	for _, raw := range []string{
		"",
		"?actor_id=",
		"?actor_id=" + validID + "&actor_key=thread%3A1",
		"?actor_id=" + validID + "&actor_id=" + validID,
		"?actor_key=thread%3A1&extra=true",
		"?actor_key=thread%3A1&actor_id=%zz",
	} {
		request := httptest.NewRequest("GET", "/"+raw, nil)
		if _, err := parseActorReadAddress(request); err == nil {
			t.Fatalf("parseActorReadAddress(%q) succeeded", raw)
		}
	}
}

func TestParseActorListRequestUsesClosedBoundedGrammar(t *testing.T) {
	request := httptest.NewRequest("GET", "/?lifecycle=failed&cursor=ac1.value&limit=100", nil)
	got, err := parseActorListRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.lifecycle != "failed" || got.cursor != "ac1.value" || got.limit != 100 {
		t.Fatalf("request = %+v", got)
	}
	defaults, err := parseActorListRequest(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.limit != actorListDefaultLimit {
		t.Fatalf("default limit = %d", defaults.limit)
	}
	for _, raw := range []string{
		"?lifecycle=",
		"?lifecycle=unknown",
		"?cursor=",
		"?limit=0",
		"?limit=101",
		"?limit=1.5",
		"?limit=1&limit=2",
		"?unknown=true",
		"?cursor=%zz",
	} {
		if _, err := parseActorListRequest(httptest.NewRequest("GET", "/"+raw, nil)); err == nil {
			t.Fatalf("parseActorListRequest(%q) succeeded", raw)
		}
	}
}

func TestActorListMalformedEncodedCursorUsesCursorCode(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/?cursor=%zz", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("actorDeclaredID", "operator.v1")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	recorder := httptest.NewRecorder()
	(&Server{}).listActorsHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), `"code":"invalid_actor_cursor"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestActorListCursorRoundTripsAndRejectsWrongScope(t *testing.T) {
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.UTC)
	raw, err := encodeActorListCursor(actorListCursor{
		ProjectID:       "prj_internal",
		EnvironmentID:   "env_internal",
		ActorDeclaredID: "operator.v1",
		CreatedAt:       createdAt.Format(time.RFC3339Nano),
		ActorID:         "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseActorListCursor(raw, "prj_internal", "env_internal", "operator.v1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.createdAt.Equal(createdAt) || got.actorID != "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("cursor = %+v", got)
	}
	for _, test := range []struct {
		raw, project, environment, declaredID string
	}{
		{raw: "ac0.value", project: "prj_internal", environment: "env_internal", declaredID: "operator.v1"},
		{raw: "ac1.not-base64", project: "prj_internal", environment: "env_internal", declaredID: "operator.v1"},
		{raw: raw, project: "other", environment: "env_internal", declaredID: "operator.v1"},
		{raw: raw, project: "prj_internal", environment: "other", declaredID: "operator.v1"},
		{raw: raw, project: "prj_internal", environment: "env_internal", declaredID: "other.v1"},
		{
			raw: "ac1." + base64.RawURLEncoding.EncodeToString([]byte(
				`{"project_id":"prj_internal","environment_id":"env_internal","actor_declared_id":"operator.v1","created_at":"2030-01-02T03:04:05.000Z","actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
			)),
			project: "prj_internal", environment: "env_internal", declaredID: "operator.v1",
		},
	} {
		if _, err := parseActorListCursor(test.raw, test.project, test.environment, test.declaredID); err == nil {
			t.Fatalf("parseActorListCursor(%q, %q, %q, %q) succeeded", test.raw, test.project, test.environment, test.declaredID)
		}
	}
}

func TestFormatDurationMillisecondsUsesLargestExactUnit(t *testing.T) {
	for value, want := range map[int64]string{
		1:           "1ms",
		1500:        "1500ms",
		90_000:      "90s",
		7_200_000:   "2h",
		172_800_000: "2d",
	} {
		got, err := formatDurationMilliseconds(value)
		if err != nil {
			t.Fatalf("formatDurationMilliseconds(%d): %v", value, err)
		}
		if got != want {
			t.Fatalf("formatDurationMilliseconds(%d) = %q, want %q", value, got, want)
		}
	}
	if _, err := formatDurationMilliseconds(0); err == nil {
		t.Fatal("formatDurationMilliseconds(0) succeeded")
	}
}

func TestProjectActorStatusNormalizesStoredManagedRunOptions(t *testing.T) {
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	expiresAt := createdAt.Add(time.Hour)
	record := actorReadRecord{
		publicID:                  "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		key:                       pgtype.Text{String: "thread:1", Valid: true},
		state:                     "open",
		expiresAt:                 pgtype.Timestamptz{Time: expiresAt, Valid: true},
		metadata:                  []byte(`{"owner":"test"}`),
		tags:                      []string{"alpha"},
		managedQueueName:          "priority",
		managedConcurrencyKey:     pgtype.Text{String: "account:1", Valid: true},
		managedPriority:           7,
		managedQueuedTTLMS:        pgtype.Int8{Int64: 90_000, Valid: true},
		managedMaxDurationMS:      7_200_000,
		managedRetryPolicyVersion: 0,
		managedRetryPolicy:        []byte(`{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":30000,"factor":2,"jitter":"full"}}`),
		managedRunMetadata:        []byte(`{"kind":"managed"}`),
		managedRunTags:            []string{"worker"},
		createdAt:                 pgtype.Timestamptz{Time: createdAt, Valid: true},
		updatedAt:                 pgtype.Timestamptz{Time: updatedAt, Valid: true},
		currentRunID:              pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		currentRunPublicID:        pgtype.Text{String: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa", Valid: true},
	}
	got, err := projectActorStatus(record)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != record.publicID || got.Key == nil || *got.Key != "thread:1" ||
		got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiresAt) ||
		got.CurrentRunID == nil || *got.CurrentRunID != "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("identity projection = %+v", got)
	}
	if got.Run.Queue != "priority" || got.Run.Priority != 7 ||
		got.Run.TTL == nil || *got.Run.TTL != "90s" ||
		got.Run.MaxDuration != "2h" ||
		got.Run.ConcurrencyKey == nil || *got.Run.ConcurrencyKey != "account:1" {
		t.Fatalf("run projection = %+v", got.Run)
	}
	if !got.Run.Retry.Enabled || got.Run.Retry.MaxAttempts == nil || *got.Run.Retry.MaxAttempts != 3 ||
		got.Run.Retry.Backoff == nil || got.Run.Retry.Backoff.MinDelay != "1s" ||
		got.Run.Retry.Backoff.MaxDelay != "30s" {
		t.Fatalf("retry projection = %+v", got.Run.Retry)
	}
	if string(got.Metadata) != `{"owner":"test"}` || string(got.Run.Metadata) != `{"kind":"managed"}` {
		t.Fatalf("metadata projection = %s / %s", got.Metadata, got.Run.Metadata)
	}
}

func TestProjectActorStatusFailsClosedOnInconsistentFailure(t *testing.T) {
	base := actorReadRecord{
		publicID:             "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		state:                "failed",
		metadata:             []byte(`{}`),
		managedQueueName:     "default",
		managedMaxDurationMS: 1,
		managedRetryPolicy:   []byte(`{"enabled":false}`),
		managedRunMetadata:   []byte(`{}`),
		createdAt:            pgtype.Timestamptz{Time: time.Now(), Valid: true},
		updatedAt:            pgtype.Timestamptz{Time: time.Now(), Valid: true},
		failureCode:          pgtype.Text{String: "invalid", Valid: true},
		failureRunID:         pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		failureRunPublicID:   pgtype.Text{String: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa", Valid: true},
	}
	if _, err := projectActorStatus(base); err == nil || !strings.Contains(err.Error(), "failure code") {
		t.Fatalf("projectActorStatus() error = %v", err)
	}
}
