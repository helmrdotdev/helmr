package controlplane

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestWorkerClaimsDoNotReplaceEpochOrStateFences(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		refresh   bool
	}{
		{"active claims", `UPDATE worker_hosts SET claim_version=claim_version+1`, true},
		{"draining claims", `UPDATE worker_hosts SET status='draining',draining_at=now(),claim_version=claim_version+1`, true},
		{"new epoch", `UPDATE worker_hosts SET current_epoch=2,claim_version=claim_version+1`, false},
		{"lost", `UPDATE worker_hosts SET status='lost',lost_at=now(),claim_version=claim_version+1`, false},
		{"termination ready", `UPDATE worker_hosts SET status='termination_ready',draining_at=now(),termination_ready_at=now(),claim_version=claim_version+1`, false},
		{"active Group claims", `UPDATE worker_groups SET claim_version=claim_version+1`, true},
		{"paused Group", `UPDATE worker_groups SET status='paused',claim_version=claim_version+1`, false},
		{"disabled Group", `UPDATE worker_groups SET status='disabled',claim_version=claim_version+1`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "assigned", time.Now())
			target := f.WorkerID
			if strings.Contains(test.sql, "worker_groups") {
				target = runtest.WorkerGroupID
			}
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql+" WHERE id=$1", target)
			worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			_, _, err := (&Server{tx: f.Pool}).claimRunLease(t.Context(), worker, pgvalue.UUID(work.LeaseID), 1)
			want := errStaleRunLeaseClaim
			if test.refresh {
				want = workergroup.ErrStaleClaims
			}
			if !errors.Is(err, want) {
				t.Fatalf("authority error=%v want=%v", err, want)
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
			if !writeStaleWorkerClaims(response, translate(workergroup.ErrStaleClaims)) || response.Code != http.StatusUnauthorized {
				t.Fatalf("claims error did not request authentication: status=%d", response.Code)
			}
		})
	}
	response := httptest.NewRecorder()
	if writeStaleWorkerClaims(response, errStaleRunLeaseClaim) || response.Body.Len() != 0 {
		t.Fatal("a stale lease was translated into an authentication refresh")
	}
}

func TestWorkerSourceErrorMappersRefreshClaimsBeforeDomainErrors(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"token":      server.writeTokenError,
		"child task": func(w http.ResponseWriter, err error) { server.writeChildTaskInvokeError(w, "test", "call", err) },
		"actor":      func(w http.ResponseWriter, err error) { server.writeWorkerActorSourceError(w, "start", "test", err) },
		"computer": func(w http.ResponseWriter, err error) {
			server.writeWorkerComputerSourceError(w, "create", "test", err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			write(response, errors.Join(workergroup.ErrStaleClaims, errStaleWorkerRunSource, errChildTaskInvokeStale, errTokenCreateAuthority))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("claims response status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
