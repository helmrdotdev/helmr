package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
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

// enroll enrolls a worker host in the named pool of the fixture's group.
func (f workerHTTPFixture) enroll(t *testing.T, pool string, resourceID string) workerapi.EnrollmentResponse {
	t.Helper()
	response := f.enrollRequest(t, f.enrollmentToken, `{"api_version":"`+workerapi.APIVersion+`","resource_id":"`+resourceID+`","pool_name":"`+pool+`"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("enrollment status = %d: %s", response.Code, response.Body.String())
	}
	var enrolled workerapi.EnrollmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	if enrolled.WorkerGroupID != f.group.ID || !strings.HasPrefix(enrolled.WorkerHostSecret, "hlmr_wi_") {
		t.Fatalf("enrolled = %+v", enrolled)
	}
	return enrolled
}

// workerHost is an enrolled worker host with the service that holds its
// epoch.
type workerHost struct {
	enrolled  workerapi.EnrollmentResponse
	serviceID string
	client    *workerclient.Client
}

func (f workerHTTPFixture) host(t *testing.T, enrolled workerapi.EnrollmentResponse) workerHost {
	t.Helper()
	host := workerHost{enrolled: enrolled, serviceID: uuid.NewV7().String()}
	client, err := workerclient.New(f.url, workerclient.WithAuth(enrolled.WorkerHostID, enrolled.WorkerHostSecret), workerclient.WithService(host.serviceID))
	if err != nil {
		t.Fatal(err)
	}
	host.client = client
	return host
}

// issue exchanges the host secret for a host credential.
func (h workerHost) issue(t *testing.T, f workerHTTPFixture) string {
	t.Helper()
	return issueWorkerHostCredential(t, f.handler, h.enrolled.WorkerHostID, h.enrolled.WorkerHostSecret, h.serviceID)
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
	if err := h.client.ReportWorkerStartupRecovery(t.Context(), workerapi.StartupRecoveryRequest{
		InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0",
		ObservedAt: time.Now().UTC(), Inventory: []string{},
	}); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
}

func TestWorkerHostLifecycleHTTP(t *testing.T) {
	f := newWorkerHTTPFixture(t)

	// The request is validated before the enrollment token.
	assertAdminError(t, f.enrollRequest(t, "not-a-token", `{"api_version":"`+workerapi.APIVersion+`","resource_id":" padded","pool_name":"default"}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.enrollRequest(t, f.enrollmentToken, `{"api_version":"`+workerapi.APIVersion+`","resource_id":"i-1","pool_name":"Default"}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.enrollRequest(t, "not-a-token", `{"api_version":"`+workerapi.APIVersion+`","resource_id":"i-1","pool_name":"default"}`), http.StatusUnauthorized, "unauthorized")
	assertAdminError(t, f.enrollRequest(t, "", `{"api_version":"`+workerapi.APIVersion+`","resource_id":"i-1","pool_name":"default"}`), http.StatusUnauthorized, "unauthorized")

	enrolled := f.enroll(t, "default", "i-lifecycle")
	service := uuid.NewV7().String()
	for name, test := range map[string]struct {
		body   workerapi.HostCredentialRequest
		status int
		code   string
	}{
		"host missing":      {body: workerapi.HostCredentialRequest{WorkerHostSecret: enrolled.WorkerHostSecret, ServiceID: service}, status: http.StatusBadRequest, code: "bad_request"},
		"host malformed":    {body: workerapi.HostCredentialRequest{WorkerHostID: "host", WorkerHostSecret: enrolled.WorkerHostSecret, ServiceID: service}, status: http.StatusBadRequest, code: "bad_request"},
		"secret blank":      {body: workerapi.HostCredentialRequest{WorkerHostID: enrolled.WorkerHostID, ServiceID: "service"}, status: http.StatusUnauthorized, code: "unauthorized"},
		"service malformed": {body: workerapi.HostCredentialRequest{WorkerHostID: enrolled.WorkerHostID, WorkerHostSecret: enrolled.WorkerHostSecret, ServiceID: "service"}, status: http.StatusBadRequest, code: "bad_request"},
		"secret wrong":      {body: workerapi.HostCredentialRequest{WorkerHostID: enrolled.WorkerHostID, WorkerHostSecret: "hlmr_wi_wrong", ServiceID: service}, status: http.StatusUnauthorized, code: "unauthorized"},
	} {
		t.Run(name, func(t *testing.T) {
			test.body.APIVersion = workerapi.APIVersion
			body, err := json.Marshal(test.body)
			if err != nil {
				t.Fatal(err)
			}
			assertAdminError(t, f.request(t, http.MethodPost, "/worker/v1/instance/credential", "", string(body)), test.status, test.code)
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
	observed, err := host.client.ObserveWorker(t.Context(), workerapi.Observation{VMPausedReason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Readiness.Instance == nil || observed.Readiness.Instance.Ready || observed.Readiness.Instance.PausedReason != "maintenance" {
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
	completion := workerapi.DrainCompletionRequest{
		InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0",
		ObservedAt: time.Now().UTC(), Inventory: []string{},
	}
	completed, err := host.client.CompleteWorkerDrain(t.Context(), completion)
	if err != nil || completed.Status != workerapi.StatusTerminationReady {
		t.Fatalf("complete drain = %+v, err = %v", completed, err)
	}
	if replayed, err := host.client.CompleteWorkerDrain(t.Context(), completion); err != nil || replayed.Status != workerapi.StatusTerminationReady {
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
