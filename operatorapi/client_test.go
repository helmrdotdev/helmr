package operatorapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
		var request DrainWorkerInstanceRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ExpectedEpoch != 3 || request.ExpectedClaimVersion != 9 {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(WorkerInstance{ID: "worker-1", State: "draining", ClaimVersion: 10})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, testOperatorToken, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.DrainWorkerInstance(context.Background(), "worker-1", DrainWorkerInstanceRequest{ExpectedEpoch: 3, ExpectedClaimVersion: 9})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "draining" || result.ClaimVersion != 10 {
		t.Fatalf("result = %+v", result)
	}
}

func TestClientReturnsTypedHTTPError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stale fence", http.StatusConflict)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, testOperatorToken, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.WorkerInstance(context.Background(), "worker-1")
	httpErr, ok := err.(*HTTPError)
	if !ok || httpErr.StatusCode != http.StatusConflict || httpErr.Body != "stale fence" {
		t.Fatalf("error = %#v", err)
	}
}

func TestClientRejectsPlaintextNonLocalControlPlane(t *testing.T) {
	for _, rawURL := range []string{"http://controlplane.example.com", "file://localhost/tmp/controlplane", "ftp://127.0.0.1/controlplane"} {
		if _, err := NewClient(rawURL, testOperatorToken); err == nil {
			t.Fatalf("unsupported Control Plane URL %q was accepted", rawURL)
		}
	}
}

func TestClientRejectsNonCanonicalOperatorToken(t *testing.T) {
	for _, token := range []string{"", "operator-token", testOperatorToken + "="} {
		if _, err := NewClient("https://controlplane.example.com", token); err == nil {
			t.Fatalf("operator token %q was accepted", token)
		}
	}
}
