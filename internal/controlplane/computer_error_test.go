package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestComputerErrorPreservesPublicStatusesAndWorkerFailures(t *testing.T) {
	keyConflict := computer.KeyConflictError{Key: "taken"}
	input := computer.ValidateKey(new(" padded "))
	for _, test := range []struct {
		name      string
		err       error
		operation computerOperation
		status    int
		code      string
		retryable bool
	}{
		{"create input", input, computerCreateOperation, http.StatusBadRequest, "invalid_computer_create", false},
		{"create not deployed", computer.ErrNotDeployed, computerCreateOperation, http.StatusNotFound, "computer_not_deployed", false},
		{"create secret", fmt.Errorf("%w: API_TOKEN", computer.ErrSecretUnavailable), computerCreateOperation, http.StatusConflict, "secret_unavailable", false},
		{"create idempotency", idempotency.ConflictError{}, computerCreateOperation, http.StatusConflict, "idempotency_conflict", false},
		{"create key", keyConflict, computerCreateOperation, http.StatusConflict, "computer_key_conflict", false},
		{"create expired", idempotency.ExpiredError{}, computerCreateOperation, http.StatusGone, "operation_expired", false},
		{"create unexpected", errors.New("database unavailable"), computerCreateOperation, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"create receipt", computer.ErrReceiptInvalid, computerCreateOperation, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"delete not found", computer.ErrNotFound, computerDeleteOperation, http.StatusNotFound, "computer_not_found", false},
		{"delete busy", computer.ErrBusy, computerDeleteOperation, http.StatusConflict, "computer_busy", true},
		{"delete idempotency", idempotency.ConflictError{}, computerDeleteOperation, http.StatusConflict, "idempotency_conflict", false},
		{"delete expired", idempotency.ExpiredError{}, computerDeleteOperation, http.StatusGone, "operation_expired", false},
		{"delete unexpected", errors.New("database unavailable"), computerDeleteOperation, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"read not found", computer.ErrNotFound, computerReadOperation, http.StatusNotFound, "computer_not_found", false},
		{"read input", computer.ValidateMembersQuery(uuid.NewV7(), uuid.NewV7(), computer.MembersQuery{Limit: 101}), computerReadOperation, http.StatusBadRequest, "invalid_computer_reference", false},
		{"read unexpected", errors.New("database unavailable"), computerReadOperation, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{log: discardTestLogger()}
			recorder := httptest.NewRecorder()
			server.writeComputerError(recorder, fmt.Errorf("operation: %w", test.err), test.operation, "test")
			if recorder.Code != test.status || decodeHTTPError(t, recorder.Body.Bytes()).Code != test.code {
				t.Fatalf("public = %d %s, want %d %s", recorder.Code, recorder.Body, test.status, test.code)
			}
			failure, ok := workerComputerFailure(test.err, test.operation)
			if test.status >= http.StatusInternalServerError {
				if ok {
					t.Fatalf("worker failure for undescribed error = %+v", failure)
				}
				return
			}
			if !ok || failure.Code != test.code || failure.Retryable != test.retryable || failure.Message != test.err.Error() {
				t.Fatalf("worker failure = %+v, %v; want %s retryable=%v", failure, ok, test.code, test.retryable)
			}
		})
	}
}

// Instance operations authenticated by a worker host report stale claims as
// 401, changed authority as 409 and rejected input as 400; other failures
// stay internal. Run-sourced aggregate operations report stale claims through
// their Run source, never as a Computer failure.
func TestComputerErrorMapsInstanceOperations(t *testing.T) {
	input := computer.ValidateKey(new(" padded "))
	internal := errors.New("database unavailable")
	for _, test := range []struct {
		name      string
		err       error
		operation computerOperation
		status    int
		code      string
	}{
		{"observation changed", computer.ErrAuthorityChanged, computerInstanceObservationOperation, http.StatusConflict, "conflict"},
		{"observation input", input, computerInstanceObservationOperation, http.StatusBadRequest, "bad_request"},
		{"observation internal", internal, computerInstanceObservationOperation, http.StatusInternalServerError, "internal_error"},
		{"claim stale claims", workergroup.ErrStaleClaims, computerInstanceClaimOperation, http.StatusUnauthorized, "unauthorized"},
		{"claim internal", internal, computerInstanceClaimOperation, http.StatusInternalServerError, "internal_error"},
		{"renewal changed", computer.ErrAuthorityChanged, computerInstanceRenewalOperation, http.StatusConflict, "conflict"},
		{"renewal stale claims", workergroup.ErrStaleClaims, computerInstanceRenewalOperation, http.StatusUnauthorized, "unauthorized"},
		{"renewal internal", internal, computerInstanceRenewalOperation, http.StatusInternalServerError, "internal_error"},
		{"run cleanup changed", computer.ErrAuthorityChanged, computerRunCleanupOperation, http.StatusConflict, "conflict"},
		{"run cleanup stale claims", workergroup.ErrStaleClaims, computerRunCleanupOperation, http.StatusUnauthorized, "unauthorized"},
		{"run cleanup internal", internal, computerRunCleanupOperation, http.StatusInternalServerError, "internal_error"},
		{"restore plan changed", computer.ErrAuthorityChanged, computerRestorePlanOperation, http.StatusConflict, "conflict"},
		{"restore plan stale claims", workergroup.ErrStaleClaims, computerRestorePlanOperation, http.StatusUnauthorized, "unauthorized"},
		{"restore plan internal", internal, computerRestorePlanOperation, http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, computerError(fmt.Errorf("operation: %w", test.err), test.operation))
			if recorder.Code != test.status || decodeHTTPError(t, recorder.Body.Bytes()).Code != test.code {
				t.Fatalf("mapped = %d %s, want %d %s", recorder.Code, recorder.Body, test.status, test.code)
			}
		})
	}
	for _, operation := range []computerOperation{computerCreateOperation, computerDeleteOperation, computerReadOperation} {
		if failure, ok := workerComputerFailure(workergroup.ErrStaleClaims, operation); ok {
			t.Fatalf("run-sourced operation %d described stale claims as %+v", operation, failure)
		}
	}
}

// The owner snapshot is the stored creation receipt and the public resource:
// both encodings must stay byte-identical.
func TestComputerSnapshotEncodesAsPublicResource(t *testing.T) {
	key := "repository"
	at := time.Date(2026, time.September, 30, 12, 0, 0, 123, time.UTC)
	snapshot := computer.Snapshot{
		Residency: "unavailable", Error: json.RawMessage(`{"code":"computer_recovery_required","message":"lost"}`),
		ID: uuid.NewV7().String(), Key: &key, SandboxID: "computer.v1", DeploymentID: uuid.NewV7().String(),
		Status: computer.StatusAvailable,
		Secrets: []secretbinding.Binding{
			{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}},
			{Name: "API_TOKEN", File: &secretbinding.File{Path: "/run/secrets/key"}},
		},
		LastActivityAt: at, CreatedAt: at, UpdatedAt: at,
	}
	owner, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(apiComputerSnapshot(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(owner, public) {
		t.Fatalf("owner snapshot %s differs from public resource %s", owner, public)
	}
	var decoded api.ComputerSnapshot
	if err := json.Unmarshal(owner, &decoded); err != nil {
		t.Fatal(err)
	}
	if roundTrip, err := json.Marshal(decoded); err != nil || !bytes.Equal(roundTrip, owner) {
		t.Fatalf("public decode of owner snapshot = %s, %v", roundTrip, err)
	}
}
