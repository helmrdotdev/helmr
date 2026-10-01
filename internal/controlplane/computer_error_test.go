package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgconn"
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
// 401, rejected input as 400, changed authority and other deterministic
// conflicts as 409 and recognized dependency unavailability as 503; other
// failures stay internal. Key delivery reports an absent or changed key as a
// conflict and a failed key provider as unavailability; initial and
// checkpoint objects report unavailable object storage as unavailability,
// while checkpoint readiness and saves keep it internal; checkpoint
// registration, readiness and failure report a rejected candidate as 400, and
// only publication treats a deterministic admission failure as a conflict.
// Run-sourced aggregate operations report stale claims through their Run
// source, never as a Computer failure.
func TestComputerErrorMapsInstanceOperations(t *testing.T) {
	input := computer.ValidateKey(new(" padded "))
	internal := errors.New("database unavailable")
	storage := fmt.Errorf("%w: %w", computer.ErrStorageUnavailable, errors.New("stat timeout"))
	provider := fmt.Errorf("%w: unwrap computer key: %w", computer.ErrKeyProviderUnavailable, errors.New("provider timeout"))
	keyUnavailable := fmt.Errorf("%w: computer source is not retained", computer.ErrKeyUnavailable)
	admission := &pgconn.PgError{Code: "23514"}
	candidate := fmt.Errorf("%w: manifest", computer.ErrCheckpointCandidate)
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
		{"key delivery stale claims", workergroup.ErrStaleClaims, computerKeyDeliveryOperation, http.StatusUnauthorized, "unauthorized"},
		{"key delivery stale claims before provider", errors.Join(workergroup.ErrStaleClaims, provider), computerKeyDeliveryOperation, http.StatusUnauthorized, "unauthorized"},
		{"key delivery changed", computer.ErrAuthorityChanged, computerKeyDeliveryOperation, http.StatusConflict, "conflict"},
		{"key delivery unavailable", keyUnavailable, computerKeyDeliveryOperation, http.StatusConflict, "conflict"},
		{"key delivery provider", provider, computerKeyDeliveryOperation, http.StatusServiceUnavailable, "service_unavailable"},
		{"key delivery input", input, computerKeyDeliveryOperation, http.StatusBadRequest, "bad_request"},
		{"key delivery internal", internal, computerKeyDeliveryOperation, http.StatusInternalServerError, "internal_error"},
		{"key delivery storage", storage, computerKeyDeliveryOperation, http.StatusInternalServerError, "internal_error"},
		{"initial object stale claims", workergroup.ErrStaleClaims, computerInitialObjectOperation, http.StatusUnauthorized, "unauthorized"},
		{"initial object storage", storage, computerInitialObjectOperation, http.StatusServiceUnavailable, "service_unavailable"},
		{"initial object conflict", computer.ObjectConflictError{}, computerInitialObjectOperation, http.StatusConflict, "conflict"},
		{"initial object changed", computer.ErrAuthorityChanged, computerInitialObjectOperation, http.StatusConflict, "conflict"},
		{"initial object input", input, computerInitialObjectOperation, http.StatusBadRequest, "bad_request"},
		{"initial object admission", admission, computerInitialObjectOperation, http.StatusInternalServerError, "internal_error"},
		{"initial object key unavailable", keyUnavailable, computerInitialObjectOperation, http.StatusInternalServerError, "internal_error"},
		{"initial object internal", internal, computerInitialObjectOperation, http.StatusInternalServerError, "internal_error"},
		{"initial version stale claims", workergroup.ErrStaleClaims, computerInitialVersionOperation, http.StatusUnauthorized, "unauthorized"},
		{"initial version input", input, computerInitialVersionOperation, http.StatusBadRequest, "bad_request"},
		{"initial version changed", computer.ErrAuthorityChanged, computerInitialVersionOperation, http.StatusConflict, "conflict"},
		{"initial version conflict", computer.ObjectConflictError{}, computerInitialVersionOperation, http.StatusConflict, "conflict"},
		{"initial version storage", storage, computerInitialVersionOperation, http.StatusInternalServerError, "internal_error"},
		{"initial version internal", internal, computerInitialVersionOperation, http.StatusInternalServerError, "internal_error"},
		{"checkpoint object changed", computer.ErrAuthorityChanged, computerCheckpointObjectOperation, http.StatusConflict, "conflict"},
		{"checkpoint object admission", admission, computerCheckpointObjectOperation, http.StatusConflict, "conflict"},
		{"checkpoint object conflict", computer.ObjectConflictError{}, computerCheckpointObjectOperation, http.StatusConflict, "conflict"},
		{"checkpoint object input", input, computerCheckpointObjectOperation, http.StatusBadRequest, "bad_request"},
		{"checkpoint object storage", storage, computerCheckpointObjectOperation, http.StatusServiceUnavailable, "service_unavailable"},
		{"checkpoint object internal", internal, computerCheckpointObjectOperation, http.StatusInternalServerError, "internal_error"},
		{"checkpoint register stale claims", workergroup.ErrStaleClaims, computerCheckpointRegisterOperation, http.StatusUnauthorized, "unauthorized"},
		{"checkpoint register candidate", candidate, computerCheckpointRegisterOperation, http.StatusBadRequest, "bad_request"},
		{"checkpoint register changed", computer.ErrAuthorityChanged, computerCheckpointRegisterOperation, http.StatusConflict, "conflict"},
		{"checkpoint register admission", admission, computerCheckpointRegisterOperation, http.StatusConflict, "conflict"},
		{"checkpoint register internal", internal, computerCheckpointRegisterOperation, http.StatusInternalServerError, "internal_error"},
		{"checkpoint ready candidate", candidate, computerCheckpointReadyOperation, http.StatusBadRequest, "bad_request"},
		{"checkpoint ready changed", computer.ErrAuthorityChanged, computerCheckpointReadyOperation, http.StatusConflict, "conflict"},
		{"checkpoint ready admission", admission, computerCheckpointReadyOperation, http.StatusConflict, "conflict"},
		{"checkpoint ready storage", storage, computerCheckpointReadyOperation, http.StatusInternalServerError, "internal_error"},
		{"checkpoint ready internal", internal, computerCheckpointReadyOperation, http.StatusInternalServerError, "internal_error"},
		{"save stale claims", workergroup.ErrStaleClaims, computerSaveOperation, http.StatusUnauthorized, "unauthorized"},
		{"save changed", computer.ErrAuthorityChanged, computerSaveOperation, http.StatusConflict, "conflict"},
		{"save admission", admission, computerSaveOperation, http.StatusConflict, "conflict"},
		{"save conflict", computer.ObjectConflictError{}, computerSaveOperation, http.StatusConflict, "conflict"},
		{"save input", input, computerSaveOperation, http.StatusBadRequest, "bad_request"},
		{"save storage", storage, computerSaveOperation, http.StatusInternalServerError, "internal_error"},
		{"save internal", internal, computerSaveOperation, http.StatusInternalServerError, "internal_error"},
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

// A worker Computer failure the worker is not told about is logged once with
// its cause and reported as internal without it; a described failure is
// reported as mapped and not logged.
func TestWriteWorkerComputerErrorLogsOnlyInternalFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		logged bool
	}{
		{"internal", errors.New("database connection reset"), http.StatusInternalServerError, true},
		{"conflict", fmt.Errorf("%w: computer source is not retained", computer.ErrKeyUnavailable), http.StatusConflict, false},
		{"provider", fmt.Errorf("%w: database connection reset", computer.ErrKeyProviderUnavailable), http.StatusServiceUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			server := &Server{log: slog.New(slog.NewJSONHandler(&logs, nil))}
			recorder := httptest.NewRecorder()
			server.writeWorkerComputerError(recorder, test.err, computerKeyDeliveryOperation, "computer source delivery failed")
			if recorder.Code != test.status || strings.Contains(recorder.Body.String(), "database connection reset") {
				t.Fatalf("response = %d %s, want %d without cause", recorder.Code, recorder.Body, test.status)
			}
			if count := strings.Count(logs.String(), "database connection reset"); test.logged != (count == 1) || count > 1 {
				t.Fatalf("logged %d times: %s", count, logs.String())
			}
		})
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
