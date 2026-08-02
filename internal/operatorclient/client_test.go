package operatorclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

const testOperatorToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestClientUsesDedicatedBearerAndExactDrainFence(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testOperatorToken {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/operator/worker-instances/worker-1/drain" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var request api.OperatorDrainWorkerInstanceRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ExpectedEpoch != 3 || request.ExpectedClaimVersion != 9 {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.OperatorWorkerInstance{ID: "worker-1", State: "draining", ClaimVersion: 10})
	}))
	defer server.Close()
	client, err := New(server.URL, testOperatorToken, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.DrainWorkerInstance(context.Background(), "worker-1", api.OperatorDrainWorkerInstanceRequest{
		ExpectedEpoch: 3, ExpectedClaimVersion: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "draining" || result.ClaimVersion != 10 {
		t.Fatalf("result = %+v", result)
	}
}

func TestClientFiltersWorkerInstancesByOpaqueProviderLocator(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/operator/worker-instances" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query()["resource_id"]; len(got) != 2 || got[0] != "host-1" || got[1] != "host-2" {
			t.Fatalf("resource_id = %v", got)
		}
		if r.URL.Query().Get("worker_group_id") != "run-workers" || r.URL.Query().Get("state") != "active" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		_ = json.NewEncoder(w).Encode(api.OperatorWorkerInstancesResponse{})
	}))
	defer server.Close()
	client, err := New(server.URL, testOperatorToken, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.WorkerInstances(context.Background(), "run-workers", []string{"host-1", "host-2"}, []string{"active"}, 10); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsPlaintextNonLocalControl(t *testing.T) {
	if _, err := New("http://control.example.com", testOperatorToken); err == nil {
		t.Fatal("plaintext non-local Control URL was accepted")
	}
}

func TestClientRejectsNonCanonicalOperatorToken(t *testing.T) {
	for _, token := range []string{"", "operator-token", testOperatorToken + "="} {
		if _, err := New("https://control.example.com", token); err == nil {
			t.Fatalf("operator token %q was accepted", token)
		}
	}
}
