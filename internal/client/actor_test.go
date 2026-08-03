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

func TestReadActorOutputUsesAPIKeyRoute(t *testing.T) {
	testReadActorOutputRoute(
		t,
		false,
		"/api/actors/operator.v1/output",
		EnvironmentScopeOptions{},
	)
}

func TestReadActorOutputUsesSessionRoute(t *testing.T) {
	testReadActorOutputRoute(
		t,
		true,
		"/api/projects/project-1/environments/env-1/actors/operator.v1/output",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func testReadActorOutputRoute(
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
		if query.Get("actor_key") != "thread:東京" ||
			query.Get("after") != "7" ||
			query.Get("limit") != "25" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ActorOutputPage{
			Records: []api.ActorOutputRecord{{
				ID:          "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
				Sequence:    8,
				Data:        json.RawMessage(`null`),
				ContentType: "application/json",
				CreatedAt:   time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
				Provenance: api.ActorOutputProvenance{
					RunID:         "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
					AttemptNumber: 1,
					DeploymentID:  "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
				},
			}},
			NextAfter: 8,
			HasMore:   true,
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
	after := int64(7)
	response, err := client.ReadActorOutput(
		context.Background(),
		"operator.v1",
		api.ActorReference{ActorKey: "thread:東京"},
		ActorOutputReadOptions{
			After:                   &after,
			Limit:                   25,
			EnvironmentScopeOptions: scope,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Records) != 1 ||
		response.Records[0].Sequence != 8 ||
		string(response.Records[0].Data) != "null" ||
		response.NextAfter != 8 ||
		!response.HasMore {
		t.Fatalf("response = %+v", response)
	}
}

func TestReadActorOutputValidatesCursorAndLimit(t *testing.T) {
	client, err := New("http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	tooLarge := int64(1 << 53)
	if _, err := client.ReadActorOutput(
		context.Background(),
		"operator.v1",
		api.ActorReference{ActorKey: "thread"},
		ActorOutputReadOptions{After: &tooLarge},
	); err == nil {
		t.Fatal("ReadActorOutput() accepted an unsafe cursor")
	}
	if _, err := client.ReadActorOutput(
		context.Background(),
		"operator.v1",
		api.ActorReference{ActorKey: "thread"},
		ActorOutputReadOptions{Limit: 101},
	); err == nil {
		t.Fatal("ReadActorOutput() accepted an oversized limit")
	}
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
	if response.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
		response.Status != api.ActorPublicStatusOpen {
		t.Fatalf("response = %+v", response)
	}
}

func actorStatusFixture() api.ActorStatus {
	return api.ActorStatus{
		ID:        "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
		Status:    api.ActorPublicStatusOpen,
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
			ActorID:    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
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
	if response.ActorID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
		!response.AcceptedAt.Equal(acceptedAt) {
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
			ActorID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
			RunID:   "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
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
		Workspace: api.WorkspaceTarget{Key: &workspaceKey},
		Input:     json.RawMessage(`null`),
	}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if response.ActorID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
		response.RunID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" {
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
			ActorID:  "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
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
	if requests != 0 {
		t.Fatalf("transport requests = %d, want 0", requests)
	}
}
