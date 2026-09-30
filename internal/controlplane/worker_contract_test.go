package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// workerContractRoutes are /worker/v1 requests a worker makes before and
// during activation, each with a body the route would otherwise accept or
// reject for another reason.
var workerContractRoutes = []struct {
	method string
	path   string
	body   string
}{
	{method: http.MethodPost, path: "/worker/v1/enrollment", body: `{"resource_id":"i-1","pool_name":"default"}`},
	{method: http.MethodPost, path: "/worker/v1/instance/token", body: `{"worker_host_id":"host","worker_host_secret":"secret","service_id":"service"}`},
	{method: http.MethodPost, path: "/worker/v1/instance/recover", body: `{"inventory_complete":true}`},
	{method: http.MethodPost, path: "/worker/v1/instance/activate", body: `{"capabilities":{}}`},
	{method: http.MethodPost, path: "/worker/v1/instance/observations", body: `{"observation":{}}`},
	{method: http.MethodGet, path: "/worker/v1/instance"},
}

func TestWorkerRoutesRejectContractMismatchBeforeEverythingElse(t *testing.T) {
	// completeServerConfig has no database: a query would fail the request.
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range workerContractRoutes {
		for name, test := range map[string]struct {
			header []string
			worker string
			body   string
		}{
			"header omitted":     {body: route.body},
			"header empty":       {header: []string{""}, body: route.body},
			"foreign contract":   {header: []string{"helmr.worker-api.v1.r0"}, worker: "helmr.worker-api.v1.r0", body: route.body},
			"repeated header":    {header: []string{workerapi.Contract, workerapi.Contract}, worker: "[" + workerapi.Contract + " " + workerapi.Contract + "]", body: route.body},
			"unknown body field": {header: []string{"helmr.worker-api.v1.r0"}, worker: "helmr.worker-api.v1.r0", body: `{"renamed_field":true}`},
		} {
			t.Run(route.path+"/"+name, func(t *testing.T) {
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(test.body))
				for _, value := range test.header {
					request.Header.Add(workerapi.ContractHeader, value)
				}
				// A token that would pass or fail authentication does not matter.
				request.Header.Set("Authorization", "Bearer not-a-token")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				assertWorkerContractMismatch(t, response, test.worker)
			})
		}
	}
}

func TestWorkerContractMismatchDoesNotSpendEnrollmentRate(t *testing.T) {
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for range workerEnrollmentPerSourceLimit + 1 {
		request := httptest.NewRequest(http.MethodPost, "/worker/v1/enrollment", strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("mismatched enrollment status = %d: %s", response.Code, response.Body.String())
		}
	}
	// A matching worker still reaches the handler: its empty body is invalid,
	// not rate limited.
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/enrollment", nil)
	request.Header.Set(workerapi.ContractHeader, workerapi.Contract)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("matching enrollment status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

func assertWorkerContractMismatch(t *testing.T, response *httptest.ResponseRecorder, worker string) {
	t.Helper()
	assertAdminError(t, response, http.StatusConflict, workerapi.ContractMismatchCode)
	body := decodeHTTPError(t, response.Body.Bytes())
	var gotWorker, gotControlPlane string
	if err := json.Unmarshal(body.Details[workerapi.ContractMismatchWorkerDetail], &gotWorker); err != nil {
		t.Fatalf("worker contract detail: %v: %s", err, response.Body.String())
	}
	if err := json.Unmarshal(body.Details[workerapi.ContractMismatchControlPlaneDetail], &gotControlPlane); err != nil {
		t.Fatalf("control plane contract detail: %v: %s", err, response.Body.String())
	}
	if gotWorker != worker || gotControlPlane != workerapi.Contract {
		t.Fatalf("contract details = %q, %q; want %q, %q", gotWorker, gotControlPlane, worker, workerapi.Contract)
	}
	if !strings.Contains(body.Message, workerapi.Contract) {
		t.Fatalf("message = %q, want it to name %q", body.Message, workerapi.Contract)
	}
}

// newWorkerRequest builds a /worker/v1 request that names this build's
// contract, as the worker client does.
func newWorkerRequest(method string, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Header.Set(workerapi.ContractHeader, workerapi.Contract)
	return request
}
