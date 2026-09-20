package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
)

const (
	testSessionID          = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
	testTurnID             = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	testHoldID             = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	testWorkspaceVersionID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
)

func TestSessionMutations(t *testing.T) {
	ctx := context.Background()
	data := json.RawMessage(`{"type":"continue"}`)
	tests := []struct {
		name, path, body, response string
		invoke                     func(*Client, EnvironmentScopeOptions, string) (any, error)
	}{
		{"send enqueued", "/send", `{"data":{"type":"continue"}}`, `{"id":"operation-1","kind":"enqueued","turn_id":"` + testTurnID + `"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.SendSession(ctx, testSessionID, api.SessionDataRequest{Data: data, IdempotencyKey: key}, s)
		}},
		{"send messaged", "/send", `{"data":{"type":"continue"}}`, `{"id":"operation-1","kind":"messaged","turn_id":"` + testTurnID + `","message_id":"message-1"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.SendSession(ctx, testSessionID, api.SessionDataRequest{Data: data, IdempotencyKey: key}, s)
		}},
		{"enqueue", "/enqueue", `{"data":{"type":"continue"}}`, `{"id":"operation-1","kind":"enqueued","turn_id":"` + testTurnID + `"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.EnqueueSession(ctx, testSessionID, api.SessionDataRequest{Data: data, IdempotencyKey: key}, s)
		}},
		{"exact message", "/turns/" + testTurnID + "/messages", `{"data":{"type":"continue"}}`, `{"id":"operation-1","turn_id":"` + testTurnID + `","message_id":"message-1","status":"accepted"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.SendSessionMessage(ctx, testSessionID, testTurnID, api.SessionDataRequest{Data: data, IdempotencyKey: key}, s)
		}},
		{"close", "/close", `{}`, `{"id":"operation-1","session_id":"` + testSessionID + `","status":"closing"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.CloseSession(ctx, testSessionID, api.CloseSessionRequest{IdempotencyKey: key}, s)
		}},
		{"interrupt", "/turns/" + testTurnID + "/interrupt", `{}`, `{"id":"operation-1","session_id":"` + testSessionID + `","turn_id":"` + testTurnID + `","hold_id":"` + testHoldID + `","status":"accepted"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.InterruptSessionTurn(ctx, testSessionID, testTurnID, api.InterruptTurnRequest{IdempotencyKey: key}, s)
		}},
		{"resume", "/resume", `{"hold_id":"` + testHoldID + `"}`, `{"id":"operation-1","session_id":"` + testSessionID + `","hold_id":"` + testHoldID + `","status":"ready"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.ResumeSession(ctx, testSessionID, api.ResumeSessionRequest{HoldID: testHoldID, IdempotencyKey: key}, s)
		}},
		{"recover outside turn", "/recover", `{"hold_id":"` + testHoldID + `","turn_id":null,"workspace_version_id":"` + testWorkspaceVersionID + `","reconciliation_ref":"incident:1"}`, `{"id":"operation-1","session_id":"` + testSessionID + `","turn_id":null,"hold_id":"` + testHoldID + `","status":"held"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			return c.RecoverSession(ctx, testSessionID, api.RecoverSessionRequest{HoldID: testHoldID, WorkspaceVersionID: testWorkspaceVersionID, ReconciliationRef: "incident:1", IdempotencyKey: key}, s)
		}},
		{"recover turn", "/recover", `{"hold_id":"` + testHoldID + `","turn_id":"` + testTurnID + `","workspace_version_id":"` + testWorkspaceVersionID + `","reconciliation_ref":"incident:1","disposition":"interrupted"}`, `{"id":"operation-1","session_id":"` + testSessionID + `","turn_id":"` + testTurnID + `","hold_id":"` + testHoldID + `","status":"held"}`, func(c *Client, s EnvironmentScopeOptions, key string) (any, error) {
			turnID := testTurnID
			return c.RecoverSession(ctx, testSessionID, api.RecoverSessionRequest{HoldID: testHoldID, TurnID: &turnID, WorkspaceVersionID: testWorkspaceVersionID, ReconciliationRef: "incident:1", Disposition: "interrupted", IdempotencyKey: key}, s)
		}},
	}
	for _, scoped := range []bool{false, true} {
		for _, test := range tests {
			for _, key := range []string{"", "caller-key"} {
				t.Run(test.name+scopeName(scoped)+"/"+key, func(t *testing.T) {
					scope, prefix := sessionRouteScope(scoped)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.URL.Path != prefix+"/sessions/"+testSessionID+test.path {
							t.Errorf("request = %s %s", r.Method, r.URL.Path)
						}
						var body map[string]json.RawMessage
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							return
						}
						var actualKey string
						if err := json.Unmarshal(body["idempotency_key"], &actualKey); err != nil {
							t.Error(err)
						}
						if key != "" && actualKey != key {
							t.Errorf("key = %q, want %q", actualKey, key)
						}
						if key == "" && ids.Validate(actualKey) != nil {
							t.Errorf("generated key = %q", actualKey)
						}
						delete(body, "idempotency_key")
						encoded, _ := json.Marshal(body)
						assertSessionJSON(t, encoded, []byte(test.body))
						w.WriteHeader(http.StatusAccepted)
						_, _ = io.WriteString(w, test.response)
					}))
					defer server.Close()
					c := sessionTestClient(t, server, scoped)
					response, err := test.invoke(c, scope, key)
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					assertSessionJSON(t, encoded, []byte(test.response))
				})
			}
		}
	}
}

func TestSessionSendInvocationKeySurvivesTransportReplay(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request api.SessionDataRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		keys = append(keys, request.IdempotencyKey)
		if r.URL.Path != "/replayed" {
			http.Redirect(w, r, "/replayed", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"id":"operation-1","kind":"enqueued","turn_id":"`+testTurnID+`"}`)
	}))
	defer server.Close()
	c := sessionTestClient(t, server, false)
	for range 2 {
		if _, err := c.SendSession(context.Background(), testSessionID, api.SessionDataRequest{Data: json.RawMessage(`null`)}, EnvironmentScopeOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 4 || keys[0] != keys[1] || keys[2] != keys[3] || keys[0] == keys[2] {
		t.Fatalf("replayed invocation keys = %v", keys)
	}
}

func TestSessionReads(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(scopeName(scoped), func(t *testing.T) {
			scope, prefix := sessionRouteScope(scoped)
			explicitResult := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s", r.Method)
				}
				switch r.URL.Path {
				case prefix + "/sessions/" + testSessionID:
					_, _ = io.WriteString(w, `{"id":"`+testSessionID+`","status":"closing","current_run_id":null,"active_turn_id":null,"dispatch":{"state":"held","hold_id":"`+testHoldID+`","reason":"recovery_required"}}`)
				case prefix + "/sessions/" + testSessionID + "/turns/" + testTurnID:
					result := ""
					if explicitResult {
						result = `,"result":null`
					}
					_, _ = io.WriteString(w, `{"id":"`+testTurnID+`","sequence":1,"status":"completed","interrupt_requested":false,"accepts_messages":false,"terminal_event_id":"event-1","workspace_version_id":"`+testWorkspaceVersionID+`"`+result+`}`)
				case prefix + "/sessions":
					if r.URL.RawQuery != "limit=5&status=closing" {
						t.Errorf("query = %s", r.URL.RawQuery)
					}
					_, _ = io.WriteString(w, `{"sessions":[{"id":"`+testSessionID+`","status":"closing"}],"next_cursor":"next"}`)
				default:
					t.Errorf("path = %s", r.URL.Path)
				}
			}))
			defer server.Close()
			c := sessionTestClient(t, server, scoped)
			session, err := c.RetrieveSession(context.Background(), testSessionID, scope)
			if err != nil {
				t.Fatal(err)
			}
			if session.Status != api.SessionStatusClosing || session.CurrentRunID != nil || session.ActiveTurnID != nil || session.Dispatch.HoldID == nil || *session.Dispatch.HoldID != testHoldID || session.Dispatch.State != "held" || session.Dispatch.Reason == nil || *session.Dispatch.Reason != "recovery_required" {
				t.Fatalf("session = %+v", session)
			}
			for _, explicit := range []bool{false, true} {
				explicitResult = explicit
				turn, err := c.RetrieveSessionTurn(context.Background(), testSessionID, testTurnID, scope)
				if err != nil {
					t.Fatal(err)
				}
				if turn.ID != testTurnID || turn.Status != "completed" || turn.WorkspaceVersionID == nil || *turn.WorkspaceVersionID != testWorkspaceVersionID {
					t.Fatalf("turn = %+v", turn)
				}
				if explicit && string(turn.Result) != "null" || !explicit && turn.Result != nil {
					t.Fatalf("explicit=%v result=%s", explicit, turn.Result)
				}
			}
			page, err := c.ListSessions(context.Background(), SessionListOptions{Statuses: []string{"closing"}, Limit: 5, EnvironmentScopeOptions: scope})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Sessions) != 1 || page.Sessions[0].ID != testSessionID || page.NextCursor != "next" {
				t.Fatalf("page = %+v", page)
			}
		})
	}
}

func TestSessionEventPages(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, test := range []struct {
			name            string
			after           int64
			limit           int32
			query, response string
			status          int
			errorCode       string
		}{
			{name: "defaults", query: "after=0&limit=100", response: `{"records":[],"next_after":0,"has_more":false,"retained_after":0}`},
			{name: "maximum", after: 7, limit: 1000, query: "after=7&limit=1000", response: `{"records":[{"id":"event-1","session_id":"` + testSessionID + `","turn_id":null,"sequence":8,"created_at":"2030-01-02T03:04:05Z","kind":"output","data":null,"provenance":null}],"next_after":8,"has_more":true,"retained_after":3}`},
			{name: "empty retains cursor", after: 8, query: "after=8&limit=100", response: `{"records":[],"next_after":8,"has_more":false,"retained_after":3}`},
			{name: "expired", after: 1, query: "after=1&limit=100", status: http.StatusGone, errorCode: "cursor_expired", response: `{"error":{"code":"cursor_expired","message":"cursor expired","details":{"retained_after":3}}}`},
			{name: "beyond end", after: 99, query: "after=99&limit=100", status: http.StatusBadRequest, errorCode: "invalid_cursor", response: `{"error":{"code":"invalid_cursor","message":"cursor exceeds end"}}`},
		} {
			t.Run(test.name+scopeName(scoped), func(t *testing.T) {
				scope, prefix := sessionRouteScope(scoped)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != prefix+"/sessions/"+testSessionID+"/events" || r.URL.RawQuery != test.query {
						t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
					}
					if test.status != 0 {
						w.WriteHeader(test.status)
					}
					_, _ = io.WriteString(w, test.response)
				}))
				defer server.Close()
				c := sessionTestClient(t, server, scoped)
				page, err := c.ReadSessionEvents(context.Background(), testSessionID, SessionEventReadOptions{After: test.after, Limit: test.limit, EnvironmentScopeOptions: scope})
				if test.errorCode != "" {
					var httpErr *httpclient.Error
					if !errors.As(err, &httpErr) || httpErr.Code != test.errorCode {
						t.Fatalf("error = %v", err)
					}
					if test.errorCode == "cursor_expired" {
						assertSessionJSON(t, httpErr.Details, []byte(`{"retained_after":3}`))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(page)
				assertSessionJSON(t, encoded, []byte(test.response))
			})
		}
	}
}

func TestStartActorRoutes(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(scopeName(scoped), func(t *testing.T) {
			scope, prefix := sessionRouteScope(scoped)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != prefix+"/actors/operator.v1/start" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var request api.StartActorRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if ids.Validate(request.IdempotencyKey) != nil || request.Workspace.ID != testWorkspaceVersionID {
					t.Errorf("request = %+v", request)
				}
				_, _ = io.WriteString(w, `{"session_id":"`+testSessionID+`","run_id":"run-1"}`)
			}))
			defer server.Close()
			c := sessionTestClient(t, server, scoped)
			response, err := c.StartActor(context.Background(), "operator.v1", api.StartActorRequest{Workspace: api.WorkspaceIDTarget{ID: testWorkspaceVersionID}}, scope)
			if err != nil {
				t.Fatal(err)
			}
			if response.SessionID != testSessionID || response.RunID != "run-1" {
				t.Fatalf("response = %+v", response)
			}
		})
	}
}

func TestSessionClientsValidateBeforeTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected transport request") }))
	defer server.Close()
	c := sessionTestClient(t, server, false)
	ctx := context.Background()
	scope := EnvironmentScopeOptions{}
	for _, invoke := range []func() error{
		func() error {
			_, err := c.SendSession(ctx, "invalid", api.SessionDataRequest{Data: json.RawMessage(`null`)}, scope)
			return err
		},
		func() error { _, err := c.SendSession(ctx, testSessionID, api.SessionDataRequest{}, scope); return err },
		func() error {
			_, err := c.SendSessionMessage(ctx, testSessionID, "invalid", api.SessionDataRequest{Data: json.RawMessage(`null`)}, scope)
			return err
		},
		func() error {
			_, err := c.ResumeSession(ctx, testSessionID, api.ResumeSessionRequest{}, scope)
			return err
		},
		func() error {
			_, err := c.RecoverSession(ctx, testSessionID, api.RecoverSessionRequest{HoldID: testHoldID, WorkspaceVersionID: testWorkspaceVersionID, ReconciliationRef: "incident", Disposition: "failed"}, scope)
			return err
		},
		func() error {
			_, err := c.ReadSessionEvents(ctx, testSessionID, SessionEventReadOptions{After: 1 << 53})
			return err
		},
		func() error {
			_, err := c.ReadSessionEvents(ctx, testSessionID, SessionEventReadOptions{After: -1})
			return err
		},
		func() error {
			_, err := c.ReadSessionEvents(ctx, testSessionID, SessionEventReadOptions{Limit: 1001})
			return err
		},
		func() error {
			_, err := c.ListSessions(ctx, SessionListOptions{Statuses: []string{"invalid"}})
			return err
		},
		func() error {
			_, err := c.ListSessions(ctx, SessionListOptions{ActorID: "operator.v1", Key: "thread:1", Statuses: []string{"open"}})
			return err
		},
		func() error { _, err := c.ListRuns(ctx, ListRunsOptions{SessionID: "invalid"}); return err },
		func() error { _, err := c.ListRuns(ctx, ListRunsOptions{Kinds: []string{"schedule"}}); return err },
	} {
		if err := invoke(); err == nil {
			t.Error("invalid request accepted")
		}
	}
}

func sessionTestClient(t *testing.T, server *httptest.Server, scoped bool) *Client {
	t.Helper()
	options := []Option{WithHTTPClient(server.Client())}
	if scoped {
		options = append(options, WithSessionScopedRoutes())
	}
	c, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func sessionRouteScope(scoped bool) (EnvironmentScopeOptions, string) {
	if scoped {
		return EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"}, "/api/projects/project-1/environments/env-1"
	}
	return EnvironmentScopeOptions{}, "/v1"
}
func scopeName(scoped bool) string {
	if scoped {
		return "/management"
	}
	return "/developer"
}
func assertSessionJSON(t *testing.T, actual, want []byte) {
	t.Helper()
	var actualValue, wantValue any
	if err := json.Unmarshal(actual, &actualValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualValue, wantValue) {
		t.Errorf("JSON = %s, want %s", actual, want)
	}
}

func actorStatusFixture() api.Session {
	return api.Session{
		ID: testSessionID, ActorID: "operator.v1", DeploymentID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
		WorkspaceID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: api.SessionStatusOpen,
		Dispatch: api.SessionDispatch{State: "ready"},
	}
}
