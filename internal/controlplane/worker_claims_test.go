package controlplane

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerClaimsDoNotReplaceEpochOrStateFences(t *testing.T) {
	for _, test := range []struct {
		name  string
		epoch pgtype.Int8
		state db.WorkerInstanceStatus
		want  error
	}{
		{"active", pgtype.Int8{Int64: 1, Valid: true}, db.WorkerInstanceStatusActive, errStaleWorkerClaims},
		{"draining", pgtype.Int8{Int64: 1, Valid: true}, db.WorkerInstanceStatusDraining, errStaleWorkerClaims},
		{"new epoch", pgtype.Int8{Int64: 2, Valid: true}, db.WorkerInstanceStatusActive, errStaleRunLeaseClaim},
		{"missing epoch", pgtype.Int8{}, db.WorkerInstanceStatusActive, errStaleRunLeaseClaim},
		{"lost", pgtype.Int8{Int64: 1, Valid: true}, db.WorkerInstanceStatusLost, errStaleRunLeaseClaim},
		{"termination ready", pgtype.Int8{Int64: 1, Valid: true}, db.WorkerInstanceStatusTerminationReady, errStaleRunLeaseClaim},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateClaimWorker(workerActor{WorkerEpoch: 1, ClaimVersion: 1}, db.WorkerInstance{
				CurrentEpoch: test.epoch, Status: test.state, ClaimVersion: 2,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("authority error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestWorkerClaimsSurviveFinalizationErrorTranslation(t *testing.T) {
	for name, translate := range map[string]func(error) error{
		"task":         func(err error) error { return staleTaskCompletion(staleRunFinalization(err)) },
		"actor":        func(err error) error { return staleActorCompletion(staleRunFinalization(err)) },
		"actor turn":   func(err error) error { return staleActorTurnCommit(staleRunFinalization(err)) },
		"actor output": staleActorOutputAppend,
		"run source":   staleWorkerRunSource,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			if !writeStaleWorkerClaims(response, translate(errStaleWorkerClaims)) || response.Code != http.StatusUnauthorized {
				t.Fatalf("claims error did not request authentication: status=%d", response.Code)
			}
		})
	}
	response := httptest.NewRecorder()
	if writeStaleWorkerClaims(response, errStaleRunLeaseClaim) || response.Body.Len() != 0 {
		t.Fatal("a stale lease was translated into an authentication refresh")
	}
}

func TestWorkerGroupClaimsDoNotReplaceStateFences(t *testing.T) {
	for _, state := range []db.WorkerGroupStatus{db.WorkerGroupStatusPaused, db.WorkerGroupStatusDisabled} {
		t.Run(state, func(t *testing.T) {
			worker, locators, authority := validRunLeaseClaimFixture()
			authority.workerGroup.Status = state
			authority.workerGroup.ClaimVersion++
			store := &runLeaseClaimStore{authority: authority}
			_, err := claimFreshTaskRunLeaseInTx(t.Context(), store, worker, authority.runLease.ID, authority.runLease.LeaseSequence, locators)
			if !errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, errStaleWorkerClaims) {
				t.Fatalf("group state error = %v, want stale lease authority", err)
			}
		})
	}
}

func TestWorkerSourceErrorMappersRefreshClaimsBeforeDomainErrors(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"token":      server.writeTokenError,
		"child task": func(w http.ResponseWriter, err error) { server.writeChildTaskInvokeError(w, "test", "call", err) },
		"actor":      func(w http.ResponseWriter, err error) { server.writeWorkerActorSourceError(w, "start", "test", err) },
		"workspace": func(w http.ResponseWriter, err error) {
			server.writeWorkerWorkspaceSourceError(w, "create", "test", err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			write(response, errors.Join(errStaleWorkerClaims, errStaleWorkerRunSource, errChildTaskInvokeStale, errTokenCreateAuthority))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("claims response status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
