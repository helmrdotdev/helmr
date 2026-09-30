package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerEnrollmentRejectsContractMismatchBeforeDatabase(t *testing.T) {
	// completeServerConfig has no database: a query would fail the request.
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{"helmr.worker-api.v0", ""} {
		request := httptest.NewRequest(http.MethodPost, "/worker/v1/enrollment",
			strings.NewReader(enrollmentBody(t, contract, "i-1", "default")))
		request.Header.Set("Authorization", "Bearer "+token.Raw)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assertWorkerContractMismatch(t, response, contract)
	}
}

func TestWorkerHostErrorMapsContractMismatch(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Server{log: discardTestLogger()}).writeWorkerHostError(recorder, "activate worker",
		workerapi.CheckContract("helmr.worker-api.v0"))
	assertWorkerContractMismatch(t, recorder, "helmr.worker-api.v0")
	if got := decodeHTTPError(t, recorder.Body.Bytes()).Message; !strings.Contains(got, workerapi.Contract) {
		t.Fatalf("message = %q, want it to name %q", got, workerapi.Contract)
	}
}

func enrollmentBody(t *testing.T, contract string, resourceID string, pool string) string {
	t.Helper()
	body, err := json.Marshal(workerapi.EnrollmentRequest{Contract: contract, ResourceID: resourceID, PoolName: pool})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertWorkerContractMismatch(t *testing.T, response *httptest.ResponseRecorder, worker string) {
	t.Helper()
	assertAdminError(t, response, http.StatusConflict, workerapi.ContractMismatchCode)
	details := decodeHTTPError(t, response.Body.Bytes()).Details
	var gotWorker, gotControlPlane string
	if err := json.Unmarshal(details[workerapi.ContractMismatchWorkerDetail], &gotWorker); err != nil {
		t.Fatalf("worker contract detail: %v: %s", err, response.Body.String())
	}
	if err := json.Unmarshal(details[workerapi.ContractMismatchControlPlaneDetail], &gotControlPlane); err != nil {
		t.Fatalf("control plane contract detail: %v: %s", err, response.Body.String())
	}
	if gotWorker != worker || gotControlPlane != workerapi.Contract {
		t.Fatalf("contract details = %q, %q; want %q, %q", gotWorker, gotControlPlane, worker, workerapi.Contract)
	}
}
