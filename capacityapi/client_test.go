package capacityapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testCapacityToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestClientUsesDedicatedBearerAndExactDrainFence(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testCapacityToken {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/capacity/v0/worker-instances/worker-1/drain" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var request DrainWorkerInstanceRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ExpectedEpoch != 3 || request.ExpectedClaimVersion != 9 {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(WorkerInstance{ID: "worker-1", Status: "draining", ClaimVersion: 10})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, testCapacityToken, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.DrainWorkerInstance(context.Background(), "worker-1", DrainWorkerInstanceRequest{ExpectedEpoch: 3, ExpectedClaimVersion: 9})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "draining" || result.ClaimVersion != 10 {
		t.Fatalf("result = %+v", result)
	}
}

func TestClientPlansCapacityAndReadsFilteredWorkerInstances(t *testing.T) {
	manifest := validTestWorkerReleaseManifest(t)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testCapacityToken {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		requests++
		switch requests {
		case 1:
			if r.Method != http.MethodPost || r.URL.RequestURI() != "/api/capacity/v0/worker-groups/group-1/plan" {
				t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
			}
			var request CapacityPlanRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Worker.ReleaseFingerprint != manifest.ReleaseFingerprint || request.MaxAdditionalWorkers != 4 {
				t.Fatalf("request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(CapacityPlanResponse{WorkerGroupID: "group-1", RecommendedAdditionalWorkers: 2})
		case 2:
			want := "/api/capacity/v0/worker-instances?limit=25&resource_id=host-a&resource_id=host-b&status=active&status=draining&worker_group_id=run-us-east-1"
			if r.Method != http.MethodGet || r.URL.RequestURI() != want {
				t.Fatalf("request = %s %s, want GET %s", r.Method, r.URL.RequestURI(), want)
			}
			_ = json.NewEncoder(w).Encode(WorkerInstancesResponse{
				WorkerInstances: []WorkerInstance{{ID: "worker-1", Status: "active"}},
			})
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, testCapacityToken, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.Plan(context.Background(), "group-1", CapacityPlanRequest{Worker: manifest, MaxAdditionalWorkers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if plan.WorkerGroupID != "group-1" || plan.RecommendedAdditionalWorkers != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	workers, err := client.WorkerInstances(
		context.Background(),
		"run-us-east-1",
		[]string{"host-a", "host-b"},
		[]WorkerInstanceStatus{WorkerInstanceStatusActive, WorkerInstanceStatusDraining},
		25,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers.WorkerInstances) != 1 || workers.WorkerInstances[0].ID != "worker-1" {
		t.Fatalf("workers = %+v", workers)
	}
}

func validTestWorkerReleaseManifest(t *testing.T) WorkerReleaseManifest {
	t.Helper()
	manifest := WorkerReleaseManifest{
		Schema: WorkerReleaseManifestSchema, WorkerVersion: "0123456789abcdef0123456789abcdef01234567", SupportsRun: true,
		Runtime:          testRuntimeProfile(t),
		Substrate:        SubstrateProfile{Format: "ext4", Contract: "helmr.substrate.ext4.v0"},
		Capacity:         ResourceVector{CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 64 << 30, VMSlots: 1},
		PerVM:            ResourceVector{CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 32 << 30},
		MaxRuntimeStarts: 1,
	}
	fingerprint, err := manifest.ExpectedFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	manifest.ReleaseFingerprint = fingerprint
	return manifest
}

func TestClientReturnsTypedHTTPError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "stale_fence", "message": "stale fence", "details": map[string]any{"claim_version": 7},
		}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, testCapacityToken, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.WorkerInstance(context.Background(), "worker-1")
	httpErr, ok := err.(*HTTPError)
	if !ok || httpErr.StatusCode != http.StatusConflict || httpErr.Code != "stale_fence" ||
		httpErr.Message != "stale fence" || string(httpErr.Details["claim_version"]) != "7" {
		t.Fatalf("error = %#v", err)
	}
}

func TestClientRejectsPlaintextNonLocalControlPlane(t *testing.T) {
	for _, rawURL := range []string{"http://controlplane.example.com", "file://localhost/tmp/controlplane", "ftp://127.0.0.1/controlplane"} {
		if _, err := NewClient(rawURL, testCapacityToken); err == nil {
			t.Fatalf("unsupported Control Plane URL %q was accepted", rawURL)
		}
	}
}

func TestClientRejectsNonCanonicalCapacityToken(t *testing.T) {
	for _, token := range []string{"", "capacity-token", testCapacityToken + "="} {
		if _, err := NewClient("https://controlplane.example.com", token); err == nil {
			t.Fatalf("capacity token %q was accepted", token)
		}
	}
}
