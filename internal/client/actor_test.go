package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

const testSessionID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"

func TestSessionRoutes(t *testing.T) {
	tests := []struct {
		name    string
		scoped  bool
		path    string
		scope   EnvironmentScopeOptions
		request func(*testing.T, *Client, EnvironmentScopeOptions)
	}{
		{
			name: "developer input", path: "/v1/sessions/" + testSessionID + "/inputs",
			request: testSendSessionInput,
		},
		{
			name: "management input", scoped: true,
			path:    "/api/projects/project-1/environments/env-1/sessions/" + testSessionID + "/inputs",
			scope:   EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
			request: testSendSessionInput,
		},
		{
			name: "developer close", path: "/v1/sessions/" + testSessionID + "/close",
			request: testCloseSession,
		},
		{
			name: "management close", scoped: true,
			path:    "/api/projects/project-1/environments/env-1/sessions/" + testSessionID + "/close",
			scope:   EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
			request: testCloseSession,
		},
		{
			name: "developer get", path: "/v1/sessions/" + testSessionID,
			request: testGetSession,
		},
		{
			name: "management get", scoped: true,
			path:    "/api/projects/project-1/environments/env-1/sessions/" + testSessionID,
			scope:   EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
			request: testGetSession,
		},
		{
			name: "developer outputs", path: "/v1/sessions/" + testSessionID + "/outputs",
			request: testReadSessionOutputs,
		},
		{
			name: "management outputs", scoped: true,
			path:    "/api/projects/project-1/environments/env-1/sessions/" + testSessionID + "/outputs",
			scope:   EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
			request: testReadSessionOutputs,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != test.path {
					t.Fatalf("path = %q, want %q", r.URL.EscapedPath(), test.path)
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path[len(r.URL.Path)-7:] == "/inputs":
					var request api.SendSessionInputRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if string(request.Input) != `{"type":"continue"}` || request.IdempotencyKey != "input-1" {
						t.Fatalf("request = %+v", request)
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(api.SessionInput{ID: "input-1", Sequence: 7})
				case r.Method == http.MethodPost:
					var request api.CloseSessionRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if request.IdempotencyKey != "close-1" {
						t.Fatalf("request = %+v", request)
					}
					_ = json.NewEncoder(w).Encode(api.SessionCloseReceipt{SessionID: testSessionID})
				case r.Method == http.MethodGet && r.URL.Path[len(r.URL.Path)-8:] == "/outputs":
					if r.URL.Query().Get("after") != "7" || r.URL.Query().Get("limit") != "25" {
						t.Fatalf("query = %q", r.URL.RawQuery)
					}
					_ = json.NewEncoder(w).Encode(api.SessionOutputPage{Records: []api.SessionOutput{{Sequence: 8}}, NextAfter: 8, HasMore: true})
				case r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(actorStatusFixture())
				default:
					t.Fatalf("method = %s", r.Method)
				}
			}))
			defer server.Close()
			options := []Option{WithHTTPClient(server.Client())}
			if test.scoped {
				options = append(options, WithSessionScopedRoutes())
			}
			client, err := New(server.URL, options...)
			if err != nil {
				t.Fatal(err)
			}
			test.request(t, client, test.scope)
		})
	}
}

func testSendSessionInput(t *testing.T, client *Client, scope EnvironmentScopeOptions) {
	t.Helper()
	response, err := client.SendSessionInput(context.Background(), testSessionID, api.SendSessionInputRequest{
		Input: json.RawMessage(`{"type":"continue"}`), IdempotencyKey: "input-1",
	}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "input-1" || response.Sequence != 7 {
		t.Fatalf("response = %+v", response)
	}
}

func testCloseSession(t *testing.T, client *Client, scope EnvironmentScopeOptions) {
	t.Helper()
	response, err := client.CloseSession(context.Background(), testSessionID, api.CloseSessionRequest{IdempotencyKey: "close-1"}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if response.SessionID != testSessionID {
		t.Fatalf("response = %+v", response)
	}
}

func testGetSession(t *testing.T, client *Client, scope EnvironmentScopeOptions) {
	t.Helper()
	response, err := client.RetrieveSession(context.Background(), testSessionID, scope)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != testSessionID || response.Status != api.SessionStatusOpen {
		t.Fatalf("response = %+v", response)
	}
}

func testReadSessionOutputs(t *testing.T, client *Client, scope EnvironmentScopeOptions) {
	t.Helper()
	after := int64(7)
	response, err := client.ReadSessionOutputs(context.Background(), testSessionID, ActorOutputReadOptions{
		After: &after, Limit: 25, EnvironmentScopeOptions: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Records) != 1 || response.Records[0].Sequence != 8 || response.NextAfter != 8 || !response.HasMore {
		t.Fatalf("response = %+v", response)
	}
}

func TestStartActorRoutes(t *testing.T) {
	for _, test := range []struct {
		name   string
		path   string
		scoped bool
		scope  EnvironmentScopeOptions
	}{
		{name: "developer", path: "/v1/actors/operator.v1/start"},
		{name: "management", path: "/api/projects/project-1/environments/env-1/actors/operator.v1/start", scoped: true, scope: EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.EscapedPath() != test.path {
					t.Fatalf("%s %s", r.Method, r.URL.EscapedPath())
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(api.StartActorResponse{SessionID: testSessionID, RunID: "run-1"})
			}))
			defer server.Close()
			options := []Option{WithHTTPClient(server.Client())}
			if test.scoped {
				options = append(options, WithSessionScopedRoutes())
			}
			client, err := New(server.URL, options...)
			if err != nil {
				t.Fatal(err)
			}
			workspaceID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
			response, err := client.StartActor(context.Background(), "operator.v1", api.StartActorRequest{Workspace: api.WorkspaceIDTarget{ID: workspaceID}}, test.scope)
			if err != nil {
				t.Fatal(err)
			}
			if response.SessionID != testSessionID {
				t.Fatalf("response = %+v", response)
			}
		})
	}
}

func TestSessionClientsValidateBeforeTransport(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendSessionInput(context.Background(), "invalid", api.SendSessionInputRequest{Input: json.RawMessage(`null`)}, EnvironmentScopeOptions{}); err == nil {
		t.Fatal("invalid Session ID was accepted")
	}
	if _, err := client.SendSessionInput(context.Background(), testSessionID, api.SendSessionInputRequest{}, EnvironmentScopeOptions{}); err == nil {
		t.Fatal("missing input was accepted")
	}
	tooLarge := int64(1 << 53)
	if _, err := client.ReadSessionOutputs(context.Background(), testSessionID, ActorOutputReadOptions{After: &tooLarge}); err == nil {
		t.Fatal("unsafe cursor was accepted")
	}
	if _, err := client.ReadSessionOutputs(context.Background(), testSessionID, ActorOutputReadOptions{Limit: 101}); err == nil {
		t.Fatal("oversized limit was accepted")
	}
	if requests != 0 {
		t.Fatalf("transport requests = %d", requests)
	}
}

func actorStatusFixture() api.Session {
	return api.Session{
		ID: testSessionID, ActorID: "operator.v1", DeploymentID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", Status: api.SessionStatusOpen,
		CreatedAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}
