package controlplane

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// workerHTTPFixture serves NewServer over HTTP with a worker group that an
// administrator created through the admin API.
type workerHTTPFixture struct {
	httpPostgresFixture
	url             string
	admin           string
	group           api.AdminWorkerGroup
	enrollmentToken string
}

func newWorkerHTTPFixture(t *testing.T) workerHTTPFixture {
	t.Helper()
	f := workerHTTPFixture{httpPostgresFixture: newSupplyHTTPFixture(t)}
	f.admin = f.adminSession(t)
	created := f.createSupplyGroup(t, f.admin, "us-east")
	f.group, f.enrollmentToken = created.WorkerGroup, created.EnrollmentToken
	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

// enrollRequest posts an enrollment request authorized by enrollmentToken.
func (f workerHTTPFixture) enrollRequest(t *testing.T, enrollmentToken string, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.request(t, http.MethodPost, "/worker/v1/enrollment", enrollmentToken, body)
}

// enrolledWorkerHost is an enrolled worker host and the machine key it
// enrolled with.
type enrolledWorkerHost struct {
	workerapi.EnrollmentResponse
	key ed25519.PrivateKey
}

// enroll enrolls a worker host with a new machine key in the named pool of the
// fixture's group.
func (f workerHTTPFixture) enroll(t *testing.T, pool string, resourceID string) enrolledWorkerHost {
	t.Helper()
	public, key := newMachineKey(t)
	body, err := json.Marshal(workerapi.EnrollmentRequest{APIVersion: workerapi.APIVersion, ResourceID: resourceID, PoolName: pool, PublicKey: public})
	if err != nil {
		t.Fatal(err)
	}
	response := f.enrollRequest(t, f.enrollmentToken, string(body))
	if response.Code != http.StatusCreated {
		t.Fatalf("enrollment status = %d: %s", response.Code, response.Body.String())
	}
	enrolled := enrolledWorkerHost{key: key}
	if err := json.Unmarshal(response.Body.Bytes(), &enrolled.EnrollmentResponse); err != nil {
		t.Fatal(err)
	}
	if enrolled.WorkerGroupID != f.group.ID || enrolled.WorkerHostID == "" {
		t.Fatalf("enrolled = %+v", enrolled.EnrollmentResponse)
	}
	return enrolled
}

// workerHost is an enrolled worker host with the service that holds its
// epoch.
type workerHost struct {
	enrolled  enrolledWorkerHost
	serviceID string
	client    *workerclient.Client
}

func (f workerHTTPFixture) host(t *testing.T, enrolled enrolledWorkerHost) workerHost {
	t.Helper()
	host := workerHost{enrolled: enrolled, serviceID: uuid.NewV7().String()}
	client, err := workerclient.New(f.url, workerclient.WithAuth(enrolled.WorkerHostID, enrolled.key), workerclient.WithService(host.serviceID), workerclient.WithNetwork(networkpolicy.Installation{}))
	if err != nil {
		t.Fatal(err)
	}
	host.client = client
	return host
}

// issue answers a challenge with the host's machine key for a host credential.
func (h workerHost) issue(t *testing.T, f workerHTTPFixture) string {
	t.Helper()
	return issueWorkerHostCredential(t, f.handler, h.enrolled.WorkerHostID, h.enrolled.key, h.serviceID)
}

// start recovers the host and activates it with capabilities, as a worker
// process does.
func (h workerHost) start(t *testing.T, capabilities workerapi.Capabilities) workerapi.StatusResponse {
	t.Helper()
	h.recover(t)
	status, err := h.client.ActivateWorker(t.Context(), capabilities)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return status
}

// recover starts the host's epoch and reports its empty startup inventory,
// observed after the epoch started.
func (h workerHost) recover(t *testing.T) {
	t.Helper()
	if err := h.client.AuthenticateWorker(t.Context()); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := h.client.ReportWorkerStartupRecovery(t.Context(), workerapi.StartupRecoveryRequest{Quarantined: []string{}}); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
}

func TestWorkerHostLifecycleHTTP(t *testing.T) {
	f := newWorkerHTTPFixture(t)

	// The request is validated before the enrollment token.
	public, _ := newMachineKey(t)
	publicKey, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	assertAdminError(t, f.enrollRequest(t, "not-a-token", `{"api_version":"`+workerapi.APIVersion+`","resource_id":" padded","pool_name":"default"}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.enrollRequest(t, f.enrollmentToken, `{"api_version":"`+workerapi.APIVersion+`","resource_id":"i-1","pool_name":"Default"}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.enrollRequest(t, f.enrollmentToken, `{"api_version":"`+workerapi.APIVersion+`","resource_id":"i-1","pool_name":"default","public_key":"c2hvcnQ="}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.enrollRequest(t, "not-a-token", `{"api_version":"`+workerapi.APIVersion+`","resource_id":"i-1","pool_name":"default","public_key":`+string(publicKey)+`}`), http.StatusUnauthorized, "unauthorized")
	assertAdminError(t, f.enrollRequest(t, "", `{"api_version":"`+workerapi.APIVersion+`","resource_id":"i-1","pool_name":"default","public_key":`+string(publicKey)+`}`), http.StatusUnauthorized, "unauthorized")

	enrolled := f.enroll(t, "default", "i-lifecycle")
	service := uuid.NewV7().String()
	for name, body := range map[string]workerapi.HostChallengeRequest{
		"host missing":      {ServiceID: service},
		"host malformed":    {WorkerHostID: "host", ServiceID: service},
		"service malformed": {WorkerHostID: enrolled.WorkerHostID, ServiceID: "service"},
	} {
		t.Run("challenge "+name, func(t *testing.T) {
			body.APIVersion = workerapi.APIVersion
			assertAdminError(t, serveWorkerJSON(t, f.handler, "/worker/v1/instance/challenge", body), http.StatusBadRequest, "bad_request")
		})
	}
	signed := signedHostCredentialRequest(t, f.handler, enrolled.WorkerHostID, enrolled.key, service)
	_, foreignKey := newMachineKey(t)
	otherService := signedHostCredentialRequest(t, f.handler, enrolled.WorkerHostID, enrolled.key, uuid.NewV7().String())
	otherService.ServiceID = service
	for name, test := range map[string]struct {
		body   workerapi.HostCredentialRequest
		status int
		code   string
	}{
		"host missing":          {body: workerapi.HostCredentialRequest{ServiceID: service, Challenge: signed.Challenge, Signature: signed.Signature}, status: http.StatusBadRequest, code: "bad_request"},
		"host malformed":        {body: workerapi.HostCredentialRequest{WorkerHostID: "host", ServiceID: service, Challenge: signed.Challenge, Signature: signed.Signature}, status: http.StatusBadRequest, code: "bad_request"},
		"service malformed":     {body: workerapi.HostCredentialRequest{WorkerHostID: enrolled.WorkerHostID, ServiceID: "service", Challenge: signed.Challenge, Signature: signed.Signature}, status: http.StatusBadRequest, code: "bad_request"},
		"challenge missing":     {body: workerapi.HostCredentialRequest{WorkerHostID: enrolled.WorkerHostID, ServiceID: service, Signature: signed.Signature}, status: http.StatusUnauthorized, code: "unauthorized"},
		"challenge for service": {body: otherService, status: http.StatusUnauthorized, code: "unauthorized"},
		"signature missing":     {body: workerapi.HostCredentialRequest{WorkerHostID: enrolled.WorkerHostID, ServiceID: service, Challenge: signed.Challenge}, status: http.StatusUnauthorized, code: "unauthorized"},
		"signature foreign":     {body: workerapi.HostCredentialRequest{WorkerHostID: enrolled.WorkerHostID, ServiceID: service, Challenge: signed.Challenge, Signature: ed25519.Sign(foreignKey, auth.HostChallengeMessage(signed.Challenge))}, status: http.StatusUnauthorized, code: "unauthorized"},
	} {
		t.Run(name, func(t *testing.T) {
			test.body.APIVersion = workerapi.APIVersion
			assertAdminError(t, serveWorkerJSON(t, f.handler, "/worker/v1/instance/credential", test.body), test.status, test.code)
		})
	}

	host := f.host(t, enrolled)
	hostCredential := host.issue(t, f)
	// A registering host is admitted only to recovery and activation.
	assertAdminError(t, f.request(t, http.MethodGet, "/worker/v1/instance", hostCredential, ""), http.StatusUnauthorized, "unauthorized")
	activated := host.start(t, validWorkerCapabilities(t))
	if activated.Status != workerapi.StatusActive || activated.WorkerHostID != enrolled.WorkerHostID || activated.WorkerGroupID != f.group.ID {
		t.Fatalf("activated = %+v", activated)
	}
	observed, err := host.client.ObserveWorker(t.Context(), workerapi.Observation{Resources: testWorkerResources(), AdmissionPausedReason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Readiness.Ready || observed.Readiness.PausedReason != "maintenance" {
		t.Fatalf("observed readiness = %+v", observed.Readiness)
	}
	if err := host.client.FenceWorker(t.Context(), "future_diagnostic"); !httpclient.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("diagnostic fence reason error = %v, want 400", err)
	}

	// Pausing the group advances its claim version: a host credential minted before the
	// transition no longer authenticates and the host re-exchanges its secret.
	hostCredential = host.issue(t, f)
	if response := f.request(t, http.MethodGet, "/worker/v1/instance", hostCredential, ""); response.Code != http.StatusOK {
		t.Fatalf("status before pause = %d: %s", response.Code, response.Body.String())
	}
	group := decodeAdmin[api.AdminWorkerGroup](t, f.request(t, http.MethodGet, "/admin/api/v1/worker-groups/"+f.group.ID, f.admin, ""), http.StatusOK)
	decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+f.group.ID+"/pause", f.admin,
		`{"expected_claim_version":`+strconv.FormatInt(group.ClaimVersion, 10)+`}`), http.StatusOK)
	assertAdminError(t, f.request(t, http.MethodGet, "/worker/v1/instance", hostCredential, ""), http.StatusUnauthorized, "unauthorized")
	if status, err := host.client.GetWorkerStatus(t.Context()); err != nil || status.Status != workerapi.StatusActive {
		t.Fatalf("status after pause = %+v, err = %v", status, err)
	}

	// Draining advances the host claim version in the same way.
	hostCredential = host.issue(t, f)
	draining, err := host.client.DrainWorker(t.Context())
	if err != nil || draining.Status != workerapi.StatusDraining {
		t.Fatalf("drain = %+v, err = %v", draining, err)
	}
	assertAdminError(t, f.request(t, http.MethodGet, "/worker/v1/instance", hostCredential, ""), http.StatusUnauthorized, "unauthorized")
	// Drain completion accepts no body.
	hostCredential = host.issue(t, f)
	assertAdminError(t, f.request(t, http.MethodPost, "/worker/v1/instance/drain/complete", hostCredential, `{}`), http.StatusBadRequest, "bad_request")
	completed, err := host.client.CompleteWorkerDrain(t.Context())
	if err != nil || completed.Status != workerapi.StatusTerminationReady {
		t.Fatalf("complete drain = %+v, err = %v", completed, err)
	}
	if replayed, err := host.client.CompleteWorkerDrain(t.Context()); err != nil || replayed.Status != workerapi.StatusTerminationReady {
		t.Fatalf("complete drain replay = %+v, err = %v", replayed, err)
	}
}

func TestWorkerHostActivationAndFenceHTTP(t *testing.T) {
	f := newWorkerHTTPFixture(t)
	first := f.host(t, f.enroll(t, "default", "i-first"))
	first.start(t, validWorkerCapabilities(t))

	// A host whose capacity differs from the sealed pool's template is refused.
	second := f.host(t, f.enroll(t, "default", "i-second"))
	second.recover(t)
	changed := validWorkerCapabilities(t)
	changed.ExecutionSlotsAvailable = 2
	if _, err := second.client.ActivateWorker(t.Context(), changed); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("mismatched activation error = %v, want 409", err)
	}

	hostCredential := first.issue(t, f)
	if err := first.client.FenceWorker(t.Context(), "worker_retired"); err != nil {
		t.Fatal(err)
	}
	// A lost fence response is replayed with the same host credential.
	if response := f.request(t, http.MethodPost, "/worker/v1/instance/fence", hostCredential, `{"reason_code":"worker_retired"}`); response.Code != http.StatusNoContent {
		t.Fatalf("fence replay status = %d: %s", response.Code, response.Body.String())
	}
	assertAdminError(t, f.request(t, http.MethodGet, "/worker/v1/instance", hostCredential, ""), http.StatusUnauthorized, "unauthorized")
}

func TestProviderTerminationFenceUsesWorkerProtocolReason(t *testing.T) {
	f := newWorkerHTTPFixture(t)
	host := f.host(t, f.enroll(t, "default", "i-terminating"))
	host.start(t, validWorkerCapabilities(t))
	if err := host.client.FenceWorker(t.Context(), "termination_drain_failed"); !httpclient.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("retired reason=%v", err)
	}
	if err := host.client.FenceWorker(t.Context(), workerapi.FenceReasonProviderTermination); err != nil {
		t.Fatal(err)
	}
	if _, err := host.client.ObserveWorker(t.Context(), workerapi.Observation{Resources: testWorkerResources()}); !httpclient.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("fenced host observation=%v", err)
	}
}

// A local probe has no old allocation for the Control Plane to fence. Its
// quarantine still cannot turn a sealed pool into a smaller available host.
func TestWorkerHostQuarantineCannotActivateReducedPoolTemplate(t *testing.T) {
	f := newWorkerHTTPFixture(t)
	first := f.host(t, f.enroll(t, "default", "healthy"))
	caps := validWorkerCapabilities(t)
	first.start(t, caps)
	retained := f.host(t, f.enroll(t, "default", "retained"))
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	supervisor, err := worker.New(worker.Config{
		ControlPlane: retained.client, Capabilities: caps, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recover: func(context.Context) (worker.RecoveryEvidence, error) {
			return worker.RecoveryEvidence{ObservedAt: time.Now(), Quarantined: []string{owner.ID}, QuarantinedOwners: []vm.Owner{owner}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := supervisor.Run(ctx); !httpclient.IsStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "activate worker:") {
		t.Fatalf("quarantine changed sealed pool capacity: %v", err)
	}
	// An ordinary later startup with clean recovery restores the unchanged
	// capabilities. This test has no actual VM custody to reconcile.
	restarted := f.host(t, retained.enrolled)
	status := restarted.start(t, caps)
	if status.Status != workerapi.StatusActive {
		t.Fatalf("clean restart: %+v", status)
	}
}

var testWorkerResourceSequence atomic.Int64

func testWorkerResources() workerapi.ResourceObservation {
	return workerapi.ResourceObservation{Sequence: testWorkerResourceSequence.Add(1), PolicyRevision: 1, ReadyLeafKeys: []workerapi.ReadyLeafKey{}}
}
