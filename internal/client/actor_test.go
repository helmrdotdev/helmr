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

func TestSendActorInputUsesAPIKeyRoute(t *testing.T) {
	testSendActorInputRoute(t, false, "/api/actors/operator.v1/input", EnvironmentScopeOptions{})
}

func TestSendActorInputUsesSessionRoute(t *testing.T) {
	testSendActorInputRoute(
		t,
		true,
		"/api/projects/project-1/environments/env-1/actors/operator.v1/input",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func TestStartActorUsesAPIKeyRoute(t *testing.T) {
	testStartActorRoute(t, false, "/api/actors/operator.v1/start", EnvironmentScopeOptions{})
}

func TestStartActorUsesSessionRoute(t *testing.T) {
	testStartActorRoute(
		t,
		true,
		"/api/projects/project-1/environments/env-1/actors/operator.v1/start",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func TestCloseActorUsesAPIKeyRoute(t *testing.T) {
	testCloseActorRoute(t, false, "/api/actors/operator.v1/close", EnvironmentScopeOptions{})
}

func TestCloseActorUsesSessionRoute(t *testing.T) {
	testCloseActorRoute(
		t,
		true,
		"/api/projects/project-1/environments/env-1/actors/operator.v1/close",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func TestGetActorStatusUsesAPIKeyRoute(t *testing.T) {
	testGetActorStatusRoute(
		t,
		false,
		"/api/actors/operator.v1/status",
		EnvironmentScopeOptions{},
	)
}

func TestGetActorStatusUsesSessionRoute(t *testing.T) {
	testGetActorStatusRoute(
		t,
		true,
		"/api/projects/project-1/environments/env-1/actors/operator.v1/status",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func TestListActorsUsesAPIKeyRoute(t *testing.T) {
	testListActorsRoute(t, false, "/api/actors/operator.v1", EnvironmentScopeOptions{})
}

func TestListActorsUsesSessionRoute(t *testing.T) {
	testListActorsRoute(
		t,
		true,
		"/api/projects/project-1/environments/env-1/actors/operator.v1",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func testGetActorStatusRoute(
	t *testing.T,
	session bool,
	wantPath string,
	scope EnvironmentScopeOptions,
) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != wantPath {
			t.Fatalf("%s %s, want GET %s", r.Method, r.URL.EscapedPath(), wantPath)
		}
		if got := r.URL.Query().Get("actor_key"); got != "thread:東京" {
			t.Fatalf("actor_key = %q", got)
		}
		_ = json.NewEncoder(w).Encode(actorStatusFixture())
	}))
	defer server.Close()

	options := []Option{WithHTTPClient(server.Client())}
	if session {
		options = append(options, WithSessionScopedRoutes())
	}
	client, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.GetActorStatus(
		context.Background(),
		"operator.v1",
		api.ActorReference{ActorKey: "thread:東京"},
		scope,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		response.Lifecycle != api.ActorLifecycleOpen {
		t.Fatalf("response = %+v", response)
	}
}

func testListActorsRoute(
	t *testing.T,
	session bool,
	wantPath string,
	scope EnvironmentScopeOptions,
) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != wantPath {
			t.Fatalf("%s %s, want GET %s", r.Method, r.URL.EscapedPath(), wantPath)
		}
		query := r.URL.Query()
		if query.Get("lifecycle") != "open" ||
			query.Get("cursor") != "ac1.cursor/value" ||
			query.Get("limit") != "25" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListActorsResponse{
			Actors:     []api.ActorStatus{actorStatusFixture()},
			NextCursor: "ac1.next",
		})
	}))
	defer server.Close()

	options := []Option{WithHTTPClient(server.Client())}
	if session {
		options = append(options, WithSessionScopedRoutes())
	}
	client, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ListActors(context.Background(), "operator.v1", ListActorsOptions{
		Lifecycle:               api.ActorLifecycleOpen,
		Cursor:                  "ac1.cursor/value",
		Limit:                   25,
		EnvironmentScopeOptions: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Actors) != 1 ||
		response.Actors[0].ID != "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		response.NextCursor != "ac1.next" {
		t.Fatalf("response = %+v", response)
	}
}

func actorStatusFixture() api.ActorStatus {
	return api.ActorStatus{
		ID:        "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		Lifecycle: api.ActorLifecycleOpen,
		Metadata:  json.RawMessage(`{}`),
		Tags:      []string{},
		Run: api.ActorManagedRunOptions{
			Queue:       "default",
			MaxDuration: "5m",
			Retry:       api.ActorManagedRetryPolicy{Enabled: false},
			Metadata:    json.RawMessage(`{}`),
			Tags:        []string{},
		},
		CreatedAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func testCloseActorRoute(t *testing.T, session bool, wantPath string, scope EnvironmentScopeOptions) {
	t.Helper()
	acceptedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != wantPath {
			t.Fatalf("%s %s, want POST %s", r.Method, r.URL.EscapedPath(), wantPath)
		}
		var request api.ActorOperationRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ActorKey != "thread:1" || request.IdempotencyKey != "close-1" {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.ActorOperationReceipt{
			ActorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa", Lifecycle: "closing",
			AcceptedAt: acceptedAt,
		})
	}))
	defer server.Close()

	options := []Option{WithHTTPClient(server.Client())}
	if session {
		options = append(options, WithSessionScopedRoutes())
	}
	client, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CloseActor(context.Background(), "operator.v1", api.ActorOperationRequest{
		ActorKey: "thread:1", IdempotencyKey: "close-1",
	}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if response.ActorID != "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		response.Lifecycle != "closing" || !response.AcceptedAt.Equal(acceptedAt) {
		t.Fatalf("response = %+v", response)
	}
}

func testStartActorRoute(t *testing.T, session bool, wantPath string, scope EnvironmentScopeOptions) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != wantPath {
			t.Fatalf("%s %s, want POST %s", r.Method, r.URL.EscapedPath(), wantPath)
		}
		var request api.StartActorRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Workspace.Key == nil || *request.Workspace.Key != "workspace:1" ||
			string(request.Input) != `null` {
			t.Fatalf("request = %+v input=%s", request, request.Input)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.StartActorResponse{
			ActorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
			RunID:   "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
	}))
	defer server.Close()

	options := []Option{WithHTTPClient(server.Client())}
	if session {
		options = append(options, WithSessionScopedRoutes())
	}
	client, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	workspaceKey := "workspace:1"
	response, err := client.StartActor(context.Background(), "operator.v1", api.StartActorRequest{
		Workspace: api.StartActorWorkspaceTarget{Key: &workspaceKey},
		Input:     json.RawMessage(`null`),
	}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if response.ActorID != "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		response.RunID != "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("response = %+v", response)
	}
}

func testSendActorInputRoute(t *testing.T, session bool, wantPath string, scope EnvironmentScopeOptions) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != wantPath {
			t.Fatalf("%s %s, want POST %s", r.Method, r.URL.EscapedPath(), wantPath)
		}
		var request api.SendActorInputRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ActorKey != "thread:1" || string(request.Input) != `{"type":"continue"}` {
			t.Fatalf("request = %+v input=%s", request, request.Input)
		}
		_ = json.NewEncoder(w).Encode(api.SendActorInputResponse{Sequence: 7})
	}))
	defer server.Close()

	options := []Option{WithHTTPClient(server.Client())}
	if session {
		options = append(options, WithSessionScopedRoutes())
	}
	client, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.SendActorInput(context.Background(), "operator.v1", api.SendActorInputRequest{
		ActorKey: "thread:1",
		Input:    json.RawMessage(`{"type":"continue"}`),
	}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if response.Sequence != 7 {
		t.Fatalf("sequence = %d, want 7", response.Sequence)
	}
}

func TestSendActorInputRejectsInvalidReferenceBeforeTransport(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []api.SendActorInputRequest{
		{Input: json.RawMessage(`null`)},
		{
			ActorID:  "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
			ActorKey: "thread:1",
			Input:    json.RawMessage(`null`),
		},
		{ActorKey: "thread:1"},
		{ActorKey: "thread:1", Input: json.RawMessage(`{"value":1,"value":2}`)},
		{ActorKey: "thread:1", Input: json.RawMessage(`"\ud800"`)},
	} {
		if _, err := client.SendActorInput(
			context.Background(),
			"operator.v1",
			request,
			EnvironmentScopeOptions{},
		); err == nil {
			t.Fatalf("SendActorInput(%+v) succeeded, want validation error", request)
		}
	}
	if requests != 0 {
		t.Fatalf("transport requests = %d, want 0", requests)
	}
}

func TestActorReadsRejectInvalidOptionsBeforeTransport(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetActorStatus(
		context.Background(),
		"operator.v1",
		api.ActorReference{},
		EnvironmentScopeOptions{},
	); err == nil {
		t.Fatal("GetActorStatus() succeeded")
	}
	for _, options := range []ListActorsOptions{
		{Lifecycle: "unknown"},
		{Limit: -1},
		{Limit: 101},
	} {
		if _, err := client.ListActors(context.Background(), "operator.v1", options); err == nil {
			t.Fatalf("ListActors(%+v) succeeded", options)
		}
	}
	if requests != 0 {
		t.Fatalf("transport requests = %d, want 0", requests)
	}
}

func TestHTTPErrorPreservesMachineCode(t *testing.T) {
	err := decodeErrorBody(
		http.StatusConflict,
		"409 Conflict",
		[]byte(`{"error":"conflict","code":"idempotency_conflict","retryable":true,"requestId":"req_1"}`),
	)
	httpError, ok := err.(*HTTPError)
	if !ok || httpError.Code != "idempotency_conflict" ||
		!httpError.Retryable || httpError.RequestID != "req_1" {
		t.Fatalf("error = %#v", err)
	}
}
