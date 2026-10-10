package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerLifecycleClient(t *testing.T) {
	paths := []string{}
	workerCredential := "worker-credential"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/worker/v1/instance/credential":
			if got := r.Header.Get("authorization"); got != "" {
				t.Fatalf("worker host credential request auth = %s", got)
			}
			var request workerapi.HostCredentialRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.WorkerHostID != "00000000-0000-0000-0000-000000000401" || request.WorkerHostSecret != "worker-secret" || request.ServiceID != "00000000-0000-0000-0000-000000000901" {
				t.Fatalf("worker host credential request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7,
				Credential:       workerCredential,
				ExpiresInSeconds: int64(time.Hour / time.Second),
			})
		case "/worker/v1/instance/activate":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerCredential {
				t.Fatalf("worker auth = %s", got)
			}
			var request workerapi.ActivateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Capabilities.Runtime.Arch != "arm64" {
				t.Fatalf("activate capabilities = %+v", request.Capabilities)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerHostID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusActive})
		case "/worker/v1/instance/drain":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerCredential {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerHostID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusDraining, ActiveInstances: 1})
		case "/worker/v1/instance/drain/complete":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerCredential {
				t.Fatalf("worker auth = %s", got)
			}
			if r.ContentLength != 0 {
				t.Fatalf("drain completion body length=%d", r.ContentLength)
			}

			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerHostID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusTerminationReady})
		case "/worker/v1/instance":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerCredential {
				t.Fatalf("worker auth = %s", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{WorkerHostID: "00000000-0000-0000-0000-000000000401", Status: workerapi.StatusDraining, ActiveInstances: 1})
		case "/worker/v1/instance/fence":
			if got := r.Header.Get("authorization"); got != "Bearer "+workerCredential {
				t.Fatalf("worker auth = %s", got)
			}
			var request workerapi.FenceRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ReasonCode != "provider_termination" {
				t.Fatalf("fence reason = %q", request.ReasonCode)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithService("00000000-0000-0000-0000-000000000901"))
	if err != nil {
		t.Fatal(err)
	}
	if status, err := client.ActivateWorker(context.Background(), workerClientCapabilities()); err != nil || status.Status != workerapi.StatusActive {
		t.Fatalf("activate status = %+v err=%v", status, err)
	}
	if status, err := client.DrainWorker(context.Background()); err != nil || status.Status != workerapi.StatusDraining || status.ActiveInstances != 1 {
		t.Fatalf("drain status = %+v err=%v", status, err)
	}
	if status, err := client.GetWorkerStatus(context.Background()); err != nil || status.Status != workerapi.StatusDraining || status.ActiveInstances != 1 {
		t.Fatalf("worker status = %+v err=%v", status, err)
	}
	if status, err := client.CompleteWorkerDrain(context.Background()); err != nil || status.Status != workerapi.StatusTerminationReady {
		t.Fatalf("complete worker drain status = %+v, err = %v", status, err)
	}
	if err := client.FenceWorker(context.Background(), "provider_termination"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, ","); got != "/worker/v1/instance/credential,/worker/v1/instance/activate,/worker/v1/instance/drain,/worker/v1/instance,/worker/v1/instance/drain/complete,/worker/v1/instance/fence" {
		t.Fatalf("paths = %s", got)
	}
}

func TestCompleteWorkerDrainRetriesTheIdenticalProofAfterAmbiguousResponse(t *testing.T) {
	attempts := 0
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/instance/credential":
			_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7, Credential: "worker-credential", ExpiresInSeconds: 3600})
		case "/worker/v1/instance/drain/complete":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, body)
			attempts++
			if attempts == 1 {
				http.Error(w, "ambiguous upstream failure", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusTerminationReady})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("worker", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.CompleteWorkerDrain(context.Background())
	if err != nil || status.Status != workerapi.StatusTerminationReady {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	if attempts != 2 || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("attempts = %d, request bodies differ: %q != %q", attempts, bodies[0], bodies[1])
	}
}

func TestFenceWorkerRetriesTheIdenticalRequestAfterAmbiguousResponse(t *testing.T) {
	attempts := 0
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/instance/credential":
			_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7, Credential: "worker-credential", ExpiresInSeconds: 3600})
		case "/worker/v1/instance/fence":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, body)
			attempts++
			if attempts == 1 {
				http.Error(w, "ambiguous upstream failure", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("worker", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.FenceWorker(context.Background(), "provider_termination"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("attempts = %d, request bodies differ: %q != %q", attempts, bodies[0], bodies[1])
	}
}

func TestWorkerClientRefreshesHostCredentialAndReplaysBufferedRequestAfterUnauthorized(t *testing.T) {
	var credentialRequests int
	var activateBodies [][]byte
	var statusRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/instance/credential":
			credentialRequests++
			_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7,
				Credential: fmt.Sprintf("worker-credential-%d", credentialRequests), ExpiresInSeconds: 3600,
			})
		case "/worker/v1/instance/activate":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			activateBodies = append(activateBodies, body)
			if r.Header.Get("authorization") == "Bearer worker-credential-1" {
				http.Error(w, `{"error":"stale credential"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusActive})
		case "/worker/v1/instance":
			statusRequests++
			if statusRequests == 1 {
				http.Error(w, `{"error":"stale group claims"}`, http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("authorization"); got != "Bearer worker-credential-3" {
				t.Fatalf("refreshed status authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusActive})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()),
		WithAuth("00000000-0000-0000-0000-000000000401", "worker-secret"),
		WithService("00000000-0000-0000-0000-000000000901"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ActivateWorker(context.Background(), workerClientCapabilities()); err != nil {
		t.Fatal(err)
	}
	if len(activateBodies) != 2 || !bytes.Equal(activateBodies[0], activateBodies[1]) {
		t.Fatalf("activate request was not replayed exactly: %q", activateBodies)
	}
	if _, err := client.GetWorkerStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if credentialRequests != 3 || statusRequests != 2 {
		t.Fatalf("credential requests=%d status requests=%d, want 3 and 2", credentialRequests, statusRequests)
	}
}

func workerClientCapabilities() workerapi.Capabilities {
	return workerapi.Capabilities{
		Runtime: vmplatform.Profile{
			ID: "sha256:runtime", Arch: "arm64", Contract: vmplatform.Contract,
			KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs",
		},
		MaxVCPUs:                  2,
		MaxMemoryMiB:              2048,
		VMMilliCPU:                2000,
		VMMemoryMiB:               2048,
		GuestEphemeralDiskBytes:   32768 << 20,
		VMGuestEphemeralDiskBytes: 32768 << 20,
		ExecutionSlotsAvailable:   1,
	}
}

// Only connection requests carry the version; ordinary API calls keep their
// existing request bodies and need no version header.
func TestWorkerConnectionAPIVersion(t *testing.T) {
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		if r.Header.Get("Helmr-Worker-Contract") != "" {
			t.Error("unexpected version header")
		}
		var body map[string]json.RawMessage
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		switch r.URL.Path {
		case "/worker/v1/enrollment", "/worker/v1/instance/credential", "/worker/v1/instance/activate":
			var version string
			if err := json.Unmarshal(body["api_version"], &version); err != nil || version != workerapi.APIVersion {
				t.Errorf("%s version = %q, error = %v", r.URL.Path, version, err)
			}
		default:
			if _, ok := body["api_version"]; ok {
				t.Errorf("unexpected version on %s", r.URL.Path)
			}
		}
		switch r.URL.Path {
		case "/worker/v1/enrollment":
			_ = json.NewEncoder(w).Encode(workerapi.EnrollmentResponse{})
		case "/worker/v1/instance/credential":
			_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7, Credential: "worker-credential", ExpiresInSeconds: 3600})
		case "/worker/v1/instance/recover":
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusActive})
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithAuth("host", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.EnrollWorker(t.Context(), "token", workerapi.EnrollmentRequest{APIVersion: "caller-value"}); err != nil {
		t.Fatal(err)
	}
	if err = client.AuthenticateWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = client.ReportWorkerStartupRecovery(t.Context(), workerapi.StartupRecoveryRequest{Quarantined: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.ActivateWorker(t.Context(), workerClientCapabilities()); err != nil {
		t.Fatal(err)
	}
	if _, err = client.ObserveWorker(t.Context(), workerapi.Observation{}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.GetWorkerStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	if seen["/worker/v1/instance/credential"] != 1 {
		t.Fatalf("host credential issues = %d", seen["/worker/v1/instance/credential"])
	}
}

func TestWorkerAPIVersionMismatchPreservesHTTPError(t *testing.T) {
	for _, phase := range []string{"enrollment", "credential", "activation"} {
		t.Run(phase, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "activation" && r.URL.Path == "/worker/v1/instance/credential" {
					_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7, Credential: "credential", ExpiresInSeconds: 3600})
					return
				}
				calls++
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": workerapi.APIVersionMismatchCode, "message": "worker version differs from control plane version"}})
			}))
			defer server.Close()
			client, err := New(server.URL, WithAuth("host", "secret"), WithService("service"))
			if err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "enrollment":
				_, err = client.EnrollWorker(t.Context(), "token", workerapi.EnrollmentRequest{})
			case "credential":
				err = client.AuthenticateWorker(t.Context())
			case "activation":
				_, err = client.ActivateWorker(t.Context(), workerClientCapabilities())
			}
			var got *httpclient.Error
			if !errors.As(err, &got) || got.StatusCode != http.StatusConflict || got.Code != workerapi.APIVersionMismatchCode || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestHostAuthorityRejectionClassification(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "credential issuance rejected", code) }))
			defer server.Close()
			client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("00000000-0000-0000-0000-000000000401", "secret"), WithService("00000000-0000-0000-0000-000000000901"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetWorkerStatus(t.Context())
			var rejected HostAuthorityRejectedError
			if got, want := errors.As(err, &rejected), code != http.StatusServiceUnavailable; got != want {
				t.Fatalf("authority rejection = %v, want %v: %v", got, want, err)
			}
			if !httpclient.IsStatus(err, code) {
				t.Fatalf("HTTP status was lost: %v", err)
			}
		})
	}
}

func TestRepeatedStaleClaimsRemainRecoverable(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/credential" {
			_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7, Credential: "credential", ExpiresInSeconds: 3600})
			return
		}
		requests++
		if requests <= 2 {
			http.Error(w, "stale claims", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusActive})
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("00000000-0000-0000-0000-000000000401", "secret"), WithService("00000000-0000-0000-0000-000000000901"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetWorkerStatus(t.Context())
	var rejected HostAuthorityRejectedError
	if !httpclient.IsStatus(err, http.StatusUnauthorized) || errors.As(err, &rejected) {
		t.Fatalf("repeated stale claims classified as authority rejection: %v", err)
	}
	if _, err := client.GetWorkerStatus(t.Context()); err != nil {
		t.Fatalf("next observation did not recover: %v", err)
	}
}

func TestHostIdentityRetainsAuthenticatedIncarnation(t *testing.T) {
	var epoch int64 = 19
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/worker/v1/instance/credential" {
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{Credential: "credential", ExpiresInSeconds: 3600, WorkerEpoch: epoch})
	}))
	defer server.Close()
	client, err := New(server.URL, WithAuth("host", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.HostIdentity(); err == nil {
		t.Fatal("unauthenticated identity was exposed")
	}
	for range 2 {
		if err := client.AuthenticateWorker(t.Context()); err != nil {
			t.Fatal(err)
		}
		host, gotEpoch, err := client.HostIdentity()
		if err != nil || host != "host" || gotEpoch != 19 {
			t.Fatalf("identity = %s/%d, %v", host, gotEpoch, err)
		}
		client.invalidateHostCredential("credential")
	}
	// The same process must not adopt a different incarnation after renewal.
	epoch = 20
	var rejected HostAuthorityRejectedError
	if err := client.AuthenticateWorker(t.Context()); !errors.As(err, &rejected) {
		t.Fatalf("changed incarnation error = %v", err)
	}
	_, gotEpoch, err := client.HostIdentity()
	if err != nil || gotEpoch != 19 {
		t.Fatalf("original incarnation lost: %d, %v", gotEpoch, err)
	}
}

func TestHostCredentialRejectsMissingEpoch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{Credential: "credential", ExpiresInSeconds: 3600})
	}))
	defer server.Close()
	client, err := New(server.URL, WithAuth("host", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AuthenticateWorker(t.Context()); err == nil {
		t.Fatal("credential without an incarnation was accepted")
	}
	if _, _, err := client.HostIdentity(); err == nil {
		t.Fatal("failed authentication established an incarnation")
	}
}
