package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestWorkerGroupErrorMapsHTTPContract(t *testing.T) {
	var input workergroup.InputError
	var conflicting workergroup.ConflictError
	for _, test := range []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{name: "stale claims", err: workergroup.ErrStaleClaims, status: http.StatusUnauthorized, code: "unauthorized", message: "worker authentication is required"},
		{name: "stale claims first", err: errors.Join(workergroup.ErrStaleClaims, workergroup.ErrHostNotFound, conflicting), status: http.StatusUnauthorized, code: "unauthorized", message: "worker authentication is required"},
		{name: "unauthenticated", err: workergroup.ErrUnauthenticated, status: http.StatusUnauthorized, code: "unauthorized", message: "worker authentication is required"},
		{name: "enrollment token", err: workergroup.ErrInvalidEnrollmentToken, status: http.StatusUnauthorized, code: "unauthorized", message: "worker enrollment token is invalid"},
		{name: "observation conflict", err: workergroup.ErrObservationConflict, status: http.StatusForbidden, code: "forbidden", message: "worker observation conflicts with this worker epoch"},
		{name: "input", err: input, status: http.StatusBadRequest, code: "bad_request"},
		{name: "invalid plan", err: fmt.Errorf("%w: pools", workergroup.ErrInvalidPlanRequest), status: http.StatusBadRequest, code: "bad_request", message: "invalid capacity plan request: pools"},
		{name: "group not found", err: workergroup.ErrGroupNotFound, status: http.StatusNotFound, code: "not_found", message: "worker group not found"},
		{name: "pool not found", err: workergroup.ErrPoolNotFound, status: http.StatusNotFound, code: "not_found", message: "worker pool not found"},
		{name: "host not found", err: workergroup.ErrHostNotFound, status: http.StatusNotFound, code: "not_found", message: "worker host not found"},
		{name: "queued demand", err: workergroup.ErrQueuedDemand, status: http.StatusConflict, code: "queued_demand_present", message: "queued demand is present"},
		{name: "conflict", err: conflicting, status: http.StatusConflict, code: "conflict"},
		{name: "region surfaced", err: fmt.Errorf("create group: %w", region.ErrNotFound), status: http.StatusNotFound, code: "not_found", message: "create group: region not found"},
		{name: "wrapped", err: fmt.Errorf("lock: %w", workergroup.ErrGroupNotFound), status: http.StatusNotFound, code: "not_found", message: "lock: worker group not found"},
		{name: "unmapped", err: errors.New("database is down"), status: http.StatusInternalServerError, code: "internal_error", message: "internal server error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&Server{log: discardTestLogger()}).writeWorkerGroupError(recorder, test.err)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code || (test.message != "" && body.Error.Message != test.message) {
				t.Fatalf("error = %+v, want %s %q", body.Error, test.code, test.message)
			}
		})
	}
}
