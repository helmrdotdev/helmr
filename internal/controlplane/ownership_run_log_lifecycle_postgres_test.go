package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func TestRunLogAppendLifecycleOwnersPostgres(t *testing.T) {
	for _, owner := range []string{"finalization", "checkpoint", "cancel"} {
		for _, ordering := range []string{"append holds Run first", "overlapping statements"} {
			t.Run(owner+"/"+ordering, func(t *testing.T) {
				f := newActorCheckpointFixture(t)
				// This is a deadlock bound, not a production latency budget.
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				action := runLogLifecycleAction(t, ctx, f, owner)
				input := db.AppendRunLogChunkParams{
					Kind: "log.stdout", Payload: []byte(`{"stream":"stdout"}`),
					LeaseFenceFingerprint: "lifecycle-receipt", RunLeaseID: f.claim.runLease.ID,
					LeaseSequence: f.claim.runLease.LeaseSequence, WorkerGroupID: pgvalue.UUID(f.worker.WorkerGroupID),
					WorkerInstanceID: pgvalue.UUID(f.worker.WorkerInstanceID), WorkerEpoch: f.worker.WorkerEpoch,
					Stream: "stdout", ObservedSeq: 1, Content: []byte("alpha"),
				}
				accepted := 0
				if ordering == "append holds Run first" {
					tx, err := f.Pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(context.Background()) }()
					q := db.New(tx)
					if row, err := q.AppendRunLogChunk(ctx, input); err != nil || !row.ReplayMatches {
						t.Fatalf("first append: %+v %v", row, err)
					}
					accepted++
					var pid int32
					if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
						t.Fatal(err)
					}
					// The append's Run lock must not acquire a higher Session lock or an
					// exclusive lease lock. The lease FK's KEY SHARE remains compatible.
					probe, err := f.Pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = probe.Rollback(context.Background()) }()
					if _, err := probe.Exec(ctx, "SELECT id FROM sessions WHERE id=$1 FOR UPDATE NOWAIT", f.sessionID); err != nil {
						t.Fatal(err)
					}
					if _, err := probe.Exec(ctx, "SELECT id FROM run_leases WHERE id=$1 FOR NO KEY UPDATE NOWAIT", input.RunLeaseID); err != nil {
						t.Fatal(err)
					}
					if err := probe.Rollback(ctx); err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- action() }()
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						var blocked bool
						if err := f.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
							t.Fatal(err)
						}
						if blocked {
							break
						}
						select {
						case err := <-done:
							t.Fatalf("owner did not wait for appended Run: %v", err)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-ticker.C:
						}
					}
					// The actual owner now holds its higher authority locks while awaiting
					// this Run. Another append must not invert that order by requesting them.
					input.ObservedSeq++
					if row, err := q.AppendRunLogChunk(ctx, input); err != nil || !row.ReplayMatches {
						t.Fatalf("append while owner waits: %+v %v", row, err)
					}
					accepted++
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				} else {
					start := make(chan struct{})
					done := make(chan error, 1)
					go func() { <-start; done <- action() }()
					close(start)
					row, err := db.New(f.Pool).AppendRunLogChunk(ctx, input)
					if err == nil {
						if !row.ReplayMatches {
							t.Fatalf("append mismatch: %+v", row)
						}
						accepted++
					} else if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("overlapping append: %v", err)
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				input.ObservedSeq = 100
				row, err := db.New(f.Pool).AppendRunLogChunk(ctx, input)
				if owner == "checkpoint" {
					if err != nil || !row.ReplayMatches {
						t.Fatalf("committed checkpoint append: %+v %v", row, err)
					}
					accepted++
				} else if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("new chunk after committed %s: %+v %v", owner, row, err)
				}
				var chunks, events int
				if err := f.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE stream_kind='run_log'), count(*) FILTER (WHERE stream_kind='event' AND kind='log.stdout') FROM telemetry_outbox WHERE run_lease_id=$1`, input.RunLeaseID).Scan(&chunks, &events); err != nil {
					t.Fatal(err)
				}
				if chunks != accepted || events != accepted {
					t.Fatalf("effects chunks=%d events=%d accepted=%d", chunks, events, accepted)
				}
			})
		}
	}
}

func runLogLifecycleAction(t *testing.T, ctx context.Context, f *actorCheckpointFixture, owner string) func() error {
	t.Helper()
	switch owner {
	case "finalization":
		request := workerapi.BeginRunFinalizationRequest{Lease: f.fence(), OperationID: uuid.NewV7().String(), Kind: workerapi.RunFinalizationCapture, ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: f.runID.String(), AttemptNumber: 1, RunLeaseID: pgvalue.UUIDString(f.claim.runLease.ID)}}
		parsed, err := parseRunFinalization(request)
		if err != nil {
			t.Fatal(err)
		}
		return func() error { _, err := f.server.beginRunFinalization(ctx, f.worker, request, parsed); return err }
	case "checkpoint":
		waitID := uuid.NewV7()
		sequence := int64(1)
		params, err := json.Marshal(workerActorInputWaitParams{SessionID: f.sessionID.String(), AfterInputSequence: sequence})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: waitID.String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: params, ActorSpeculativeInputSequence: &sequence})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseRunLeaseFence(f.fence())
		if err != nil {
			t.Fatal(err)
		}
		return func() error {
			request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)).WithContext(context.WithValue(ctx, workerContextKey{}, f.worker))
			response := httptest.NewRecorder()
			f.server.workerCreateRunWait(response, request)
			if response.Code != http.StatusOK && response.Code != http.StatusNoContent {
				return fmt.Errorf("create wait: %d %s", response.Code, response.Body.String())
			}
			_, err := f.server.requestWorkerRunWaitCheckpoint(ctx, f.worker, f.fence(), parsed, waitID)
			return err
		}
	case "cancel":
		principal := auth.Actor{OrgID: f.OrgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleOwner, ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String(), Permissions: []auth.Permission{auth.PermissionRunsManage, auth.PermissionRunsRead}}
		request := runCancellationRequest(t, f.runID.String(), principal)
		request = request.WithContext(context.WithValue(context.WithValue(ctx, actorContextKey{}, principal), chi.RouteCtxKey, chi.RouteContext(request.Context())))
		return func() error {
			response := httptest.NewRecorder()
			f.server.cancelRunHTTP(response, request)
			if response.Code != http.StatusOK {
				return fmt.Errorf("cancel: %d %s", response.Code, response.Body.String())
			}
			return nil
		}
	default:
		t.Fatalf("unknown owner %s", owner)
		return nil
	}
}
