package controlplane

import (
	"context"
	"encoding/json"
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
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestSessionCommandEnvelopePreservesApplicationJSONAndRejectsAmbiguity(t *testing.T) {
	for _, data := range []string{`null`, `false`, `[1,"text"]`, `{"type":"update_constraints","custom":{"priority":2}}`} {
		var body api.SessionDataRequest
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"data":`+data+`}`))
		if err := decodeSessionCommand(r, &body); err != nil {
			t.Fatal(err)
		}
		if err := api.ValidateSessionDataRequest(body); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(body.Data) {
			t.Fatalf("lost data: %s", body.Data)
		}
	}
	for _, raw := range []string{
		`{"data":null,"mode":"enqueue"}`, `{"data":0,"data":1}`, `{"data":{"a":1,"a":2}}`,
		`{"data":"\ud800"}`, `{"data":null} {}`, `null`, `[]`,
		`{"data":0,"idempotency_key":null}`, `{"data":0,"idempotency_key":" "}`,
	} {
		var body api.SessionDataRequest
		if err := decodeSessionCommand(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw)), &body); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestSessionRecoveryRequiresExplicitNullableTarget(t *testing.T) {
	hold, version := uuid.NewV7().String(), uuid.NewV7().String()
	base := `"hold_id":"` + hold + `","workspace_version_id":"` + version + `","reconciliation_ref":"operator:ticket-42"`
	for _, test := range []struct {
		raw   string
		valid bool
	}{
		{`{` + base + `}`, false},
		{`{` + base + `,"turn_id":null}`, true},
		{`{` + base + `,"turn_id":null,"disposition":"failed"}`, false},
		{`{` + base + `,"turn_id":null,"disposition":null}`, false},
		{`{` + base + `,"turn_id":null,"disposition":""}`, false},
		{`{` + base + `,"turn_id":"` + uuid.NewV7().String() + `"}`, false},
		{`{` + base + `,"turn_id":"` + uuid.NewV7().String() + `","disposition":"failed"}`, true},
	} {
		_, err := decodeRecoverSessionCommand(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.raw)))
		if (err == nil) != test.valid {
			t.Fatalf("body=%s error=%v", test.raw, err)
		}
	}
}

func TestSessionEventCursorValidatesClosedQueryAndSafeIntegers(t *testing.T) {
	after, limit, err := parseSessionEventPageOptions("")
	if err != nil || after != 0 || limit != 100 {
		t.Fatalf("defaults=%d,%d,%v", after, limit, err)
	}
	if _, limit, err := parseSessionEventPageOptions("after=0&limit=1000"); err != nil || limit != 1000 {
		t.Fatalf("maximum=%d,%v", limit, err)
	}
	for _, raw := range []string{"after=-1", "after=9007199254740992", "after=1.2", "after=1&after=2", "limit=0", "limit=1001", "limit=", "cursor=1", "after=%"} {
		if _, _, err := parseSessionEventPageOptions(raw); err == nil {
			t.Fatalf("accepted query %q", raw)
		}
	}
	recorder := httptest.NewRecorder()
	(&Server{}).writeSessionOperationError(recorder, &session.OperationError{Code: "cursor_expired", RetainedAfter: 17})
	body := decodeHTTPError(t, recorder.Body.Bytes())
	if recorder.Code != http.StatusGone || body.Code != "cursor_expired" || string(body.Details["retained_after"]) != "17" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionRecoveryAuthorizesBeforeResourceLookup(t *testing.T) {
	for _, principal := range []auth.Actor{
		{Kind: auth.ActorKindSession, Role: auth.RoleDeveloper},
		{Kind: auth.ActorKindSession, Role: auth.RoleViewer},
		{Kind: auth.ActorKindAPIKey, Role: auth.RoleAdmin, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String()},
		{Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(), Permissions: []auth.Permission{auth.PermissionSessionsRecover}},
	} {
		raw := `{"hold_id":"` + uuid.NewV7().String() + `","turn_id":null,"workspace_version_id":"` + uuid.NewV7().String() + `","reconciliation_ref":"ticket:42"}`
		r := sessionLifecycleRequest(raw, principal, "not-a-session", "")
		recorder := httptest.NewRecorder()
		(&Server{}).recoverSessionHTTP(recorder, r)
		if recorder.Code != http.StatusForbidden || decodeHTTPError(t, recorder.Body.Bytes()).Code != "forbidden" {
			t.Fatalf("principal=%+v status=%d body=%s", principal, recorder.Code, recorder.Body.String())
		}
	}
}

func TestSessionDataRequestBoundsCanonicalDataAndTransport(t *testing.T) {
	for _, test := range []struct {
		body string
		want int
	}{
		{`{"data":"` + strings.Repeat(`\u0061`, (1<<20)-2) + `"}`, http.StatusForbidden},
		{`{"data":"` + strings.Repeat("a", 1<<20) + `"}`, http.StatusRequestEntityTooLarge},
		{strings.Repeat(" ", int(sessionDataBodyLimit)+1), http.StatusRequestEntityTooLarge},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
		r.ContentLength = -1
		w := httptest.NewRecorder()
		limitRequestBody(sessionDataBodyLimit)(http.HandlerFunc((&Server{}).sendSessionHTTP)).ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("status=%d want=%d body=%s", w.Code, test.want, w.Body.String())
		}
	}
}

func TestSessionTurnProjectionPreservesAbsentResultAndJSONNull(t *testing.T) {
	for _, test := range []struct {
		data    string
		present bool
	}{
		{`{}`, false},
		{`{"result":null}`, true},
	} {
		view := session.TurnView{
			Turn:          db.SessionTurn{ID: pgvalue.UUID(uuid.NewV7()), SessionID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`null`), Status: "completed"},
			TerminalEvent: &db.SessionEvent{ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(test.data)},
		}
		result, err := projectSessionTurn(view)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		value, present := envelope["result"]
		if present != test.present || present && string(value) != "null" || result.TerminalEventID == nil || result.WorkspaceVersionID != nil || envelope["workspace_version_id"] != nil {
			t.Fatalf("terminal view lost presence: %s", raw)
		}
	}
}

func sessionLifecycleRequest(body string, principal auth.Actor, sessionID, turnID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("sessionID", sessionID)
	if turnID != "" {
		route.URLParams.Add("turnID", turnID)
	}
	if principal.Kind == auth.ActorKindSession {
		route.URLParams.Add("projectID", principal.ProjectID)
		route.URLParams.Add("environmentID", principal.EnvironmentID)
	}
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return r.WithContext(ctx)
}
