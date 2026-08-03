package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

const (
	testActorID     = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
	testWorkspaceID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
)

func TestActorStartPreservesIdentityInputAndRunTemplate(t *testing.T) {
	var request api.StartActorRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actors/operator.v1/start" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.StartActorResponse{
			ActorID: testActorID,
			RunID:   "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"actor", "start", "operator.v1",
		"--workspace", testWorkspaceID,
		"--key", "thread:東京",
		"--input-json", "null",
		"--idempotency-key", "actor:start:1",
		"--queue", "agents",
		"--concurrency-key", "thread:東京",
		"--priority", "3",
		"--ttl", "10m",
		"--retry-json", `{"max_attempts":3}`,
		"--metadata-json", `{"customer":"test"}`,
		"--tag", "interactive",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "actor_id: "+testActorID+"\nrun_id: 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31\n" {
		t.Fatalf("output = %q", out.String())
	}
	if request.Key == nil || *request.Key != "thread:東京" ||
		string(request.Input) != "null" ||
		request.Workspace.ID == nil || *request.Workspace.ID != testWorkspaceID ||
		request.IdempotencyKey != "actor:start:1" {
		t.Fatalf("request = %+v", request)
	}
	if request.Run == nil ||
		request.Run.Queue != "agents" ||
		request.Run.ConcurrencyKey == nil ||
		*request.Run.ConcurrencyKey != "thread:東京" ||
		request.Run.Priority != 3 ||
		request.Run.TTL != "10m" ||
		request.Run.Retry == nil ||
		request.Run.Retry.MaxAttempts == nil ||
		*request.Run.Retry.MaxAttempts != 3 ||
		string(request.Run.Metadata) != `{"customer":"test"}` ||
		strings.Join(request.Run.Tags, ",") != "interactive" {
		t.Fatalf("run template = %+v", request.Run)
	}
}

func TestActorStartDistinguishesOmittedInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["input"]; ok {
			t.Fatalf("omitted input was serialized: %+v", raw)
		}
		_ = json.NewEncoder(w).Encode(api.StartActorResponse{ActorID: testActorID})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"actor", "start", "operator.v1", "--workspace", testWorkspaceID})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestActorAddressedCommandsUseDeclaredIDAndOneAddress(t *testing.T) {
	currentRunID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	acceptedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/actors/operator.v1/status":
			if r.URL.Query().Get("actor_key") != "thread:東京" {
				t.Fatalf("status query = %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.ActorStatus{
				ID: testActorID, Status: api.ActorPublicStatusOpen,
				CurrentRunID: &currentRunID,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/actors/operator.v1/input":
			var request api.SendActorInputRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ActorID != testActorID ||
				string(request.Input) != "null" ||
				request.IdempotencyKey != "input:1" {
				t.Fatalf("input request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.SendActorInputResponse{Sequence: 8})
		case r.Method == http.MethodPost && r.URL.Path == "/api/actors/operator.v1/close":
			var request api.ActorOperationRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ActorID != testActorID || request.IdempotencyKey != "close:1" {
				t.Fatalf("close request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.ActorOperationReceipt{
				ActorID: testActorID, AcceptedAt: acceptedAt,
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"actor", "get", "operator.v1", "--key", "thread:東京"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "actor_status: open") ||
		!strings.Contains(out.String(), "run_id: "+currentRunID) {
		t.Fatalf("get output = %q", out.String())
	}

	out.Reset()
	cmd = newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"actor", "input", "send", "operator.v1",
		"--id", testActorID,
		"--input-json", "null",
		"--idempotency-key", "input:1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "sequence: 8\n" {
		t.Fatalf("input output = %q", out.String())
	}

	out.Reset()
	cmd = newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"actor", "close", "operator.v1",
		"--id", testActorID,
		"--idempotency-key", "close:1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "actor_id: "+testActorID) ||
		!strings.Contains(out.String(), "accepted_at: 2030-01-02T03:04:05Z") {
		t.Fatalf("close output = %q", out.String())
	}
}

func TestActorOutputReadsFiniteSequencePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/actors/operator.v1/output" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("actor_id") != testActorID ||
			r.URL.Query().Get("after") != "7" ||
			r.URL.Query().Get("limit") != "1" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ActorOutputPage{
			Records: []api.ActorOutputRecord{{
				ID:       "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
				Sequence: 8,
				Data:     json.RawMessage(`{"message":"ready"}`),
			}},
			NextAfter: 8,
			HasMore:   true,
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"actor", "output", "read", "operator.v1",
		"--id", testActorID,
		"--after", "7",
		"--limit", "1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "8\t{\"message\":\"ready\"}\nnext_after: 8\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestActorCommandsRejectInvalidAddressAndMissingInput(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "http://127.0.0.1")
	t.Setenv(helmrAPIKeyEnv, "test-key")
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"actor", "get", "operator.v1"}, "exactly one of --id or --key"},
		{[]string{"actor", "get", "operator.v1", "--id", " " + testActorID}, "invalid actor ID"},
		{[]string{"actor", "get", "operator.v1", "--id", testActorID, "--key", "thread"}, "exactly one of --id or --key"},
		{[]string{"actor", "input", "send", "operator.v1", "--input-json", "null"}, "exactly one of --id or --key"},
		{[]string{"actor", "input", "send", "operator.v1", "--id", testActorID, "--key", "thread", "--input-json", "null"}, "exactly one of --id or --key"},
		{[]string{"actor", "input", "send", "operator.v1", "--id", testActorID}, "--input-file or --input-json is required"},
		{[]string{"actor", "output", "read", "operator.v1"}, "exactly one of --id or --key"},
		{[]string{"actor", "output", "read", "operator.v1", "--id", testActorID, "--key", "thread"}, "exactly one of --id or --key"},
		{[]string{"actor", "output", "read", "operator.v1", "--id", testActorID, "--limit", "0"}, "--limit must be in [1,100]"},
		{[]string{"actor", "output", "read", "operator.v1", "--id", testActorID, "--limit", "101"}, "limit must be in [1,100]"},
		{[]string{"actor", "close", "operator.v1"}, "exactly one of --id or --key"},
		{[]string{"actor", "close", "operator.v1", "--id", testActorID, "--key", "thread"}, "exactly one of --id or --key"},
	} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(test.args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("args %v error = %v, want %q", test.args, err, test.want)
		}
	}
}

func TestActorOutputDoesNotExposeStreamingOrOpaqueCursorFlags(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "http://127.0.0.1")
	t.Setenv(helmrAPIKeyEnv, "test-key")
	for _, flag := range []string{"--follow", "--cursor"} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{
			"actor", "output", "read", "operator.v1",
			"--id", testActorID,
			flag, "value",
		})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("%s error = %v", flag, err)
		}
	}
}
