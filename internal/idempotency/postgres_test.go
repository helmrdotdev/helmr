package idempotency_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	retry "github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func runRetry(ctx context.Context, txb db.TxBeginner, request *retry.Request, fn func(pgx.Tx, retry.Result) (retry.Completion, error)) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error { return retry.Run(ctx, tx, request, fn) })
}

func deploymentRequest(t *testing.T, f agenttest.Fixture, key string) *retry.Request {
	t.Helper()
	var project uuid.UUID
	var digest string
	if err := f.Pool.QueryRow(t.Context(), `SELECT e.project_id,d.bundle_digest FROM environments e JOIN deployments d ON d.environment_id=e.id WHERE e.id=$1 AND d.id=$2`, f.Environment, f.Deployment).Scan(&project, &digest); err != nil {
		t.Fatal(err)
	}
	request, err := retry.NewDeploymentFinalizeRequest(f.Environment, project, key, retry.DeploymentFinalizeFingerprint{BundleDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	return request
}
func accept(t *testing.T, f agenttest.Fixture, request *retry.Request, target retry.Target) db.IdempotencyReceipt {
	t.Helper()
	var claim db.IdempotencyReceipt
	if err := runRetry(t.Context(), f.Pool, request, func(tx pgx.Tx, r retry.Result) (retry.Completion, error) {
		claim = r.Claim
		return retry.Completion{Target: target, Receipt: []byte(`{"accepted":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.Pool.Query(t.Context(), `SELECT * FROM idempotency_receipts WHERE environment_id=$1 AND id=$2`, claim.EnvironmentID, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.IdempotencyReceipt])
	if err != nil {
		t.Fatal(err)
	}
	return stored
}
func acquireError(t *testing.T, f agenttest.Fixture, request *retry.Request) error {
	t.Helper()
	return runRetry(t.Context(), f.Pool, request, func(tx pgx.Tx, r retry.Result) (retry.Completion, error) {
		return retry.Completion{}, nil
	})
}
func TestPlatformConcurrentRetryCommitsOneReceipt(t *testing.T) {
	f := agenttest.New(t)
	request := deploymentRequest(t, f, "concurrent")
	var wg sync.WaitGroup
	results := make(chan retry.Result, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			var r retry.Result
			err := runRetry(t.Context(), f.Pool, request, func(tx pgx.Tx, acquired retry.Result) (retry.Completion, error) {
				r = acquired
				return retry.Completion{Target: retry.Target{DeploymentID: f.Deployment}, Receipt: []byte(`{"accepted":true}`)}, nil
			})
			results <- r
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id uuid.UUID
	fresh := 0
	for result := range results {
		if id == uuid.Nil() {
			id = pgvalue.MustUUIDValue(result.Claim.ID)
		}
		if pgvalue.MustUUIDValue(result.Claim.ID) != id {
			t.Fatal("response loss admitted another identity")
		}
		if result.New {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("new receipts %d", fresh)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_receipts WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts %d: %v", count, err)
	}
}
func TestPlatformIncompleteTransactionAndWrongTargetRollBack(t *testing.T) {
	for _, mode := range []string{"unfinished", "wrong-family", "foreign-target"} {
		t.Run(mode, func(t *testing.T) {
			f := agenttest.New(t)
			request := deploymentRequest(t, f, mode)
			err := runRetry(t.Context(), f.Pool, request, func(tx pgx.Tx, r retry.Result) (retry.Completion, error) {
				if _, err := tx.Exec(t.Context(), `UPDATE computers SET key='partial' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer); err != nil {
					return retry.Completion{}, err
				}
				if mode == "unfinished" {
					return retry.Completion{}, nil
				}
				target := retry.Target{ComputerID: f.Computer}
				if mode == "foreign-target" {
					target = retry.Target{DeploymentID: uuid.NewV7()}
				}
				return retry.Completion{Target: target, Receipt: []byte(`{}`)}, nil
			})
			if err == nil {
				t.Fatal("incomplete or mistyped result committed")
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_receipts WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial claim retained %d: %v", count, err)
			}
			var partial bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT COALESCE(key='partial',false) FROM computers WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer).Scan(&partial); err != nil || partial {
				t.Fatalf("mutation survived: %v, %v", partial, err)
			}
			accept(t, f, request, retry.Target{DeploymentID: f.Deployment})
		})
	}
}
func TestPlatformExpiredIdentityCannotExecuteBeforeOrAfterCollection(t *testing.T) {
	f := agenttest.New(t)
	request := deploymentRequest(t, f, "expired")
	claim := accept(t, f, request, retry.Target{DeploymentID: f.Deployment})
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE idempotency_receipts SET accepted_at=accepted_at-interval '31 days' WHERE environment_id=$1 AND id=$2`, f.Environment, claim.ID)
	var expired retry.ExpiredError
	if err := acquireError(t, f, request); !errors.As(err, &expired) {
		t.Fatalf("uncollected expiry: %v", err)
	}
	if count, err := db.New(f.Pool).PruneExpiredIdempotencyReceipts(t.Context(), 100); err != nil || count != 1 {
		t.Fatalf("prune %d: %v", count, err)
	}
	if err := acquireError(t, f, request); !errors.As(err, &expired) {
		t.Fatalf("collected expiry: %v", err)
	}
	var project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT project_id FROM environments WHERE id=$1`, f.Environment).Scan(&project); err != nil {
		t.Fatal(err)
	}
	different, _ := retry.NewDeploymentFinalizeRequest(f.Environment, project, "expired", retry.DeploymentFinalizeFingerprint{BundleDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"})
	var conflict retry.ConflictError
	if err := acquireError(t, f, different); !errors.As(err, &conflict) {
		t.Fatalf("expired conflict evidence: %v", err)
	}
}

func TestPlatformOwnedSavepointRollsBackIgnoredFailure(t *testing.T) {
	for _, mode := range []string{"error", "panic", "cancel", "missing-receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := agenttest.New(t)
			request := deploymentRequest(t, f, mode)
			err := db.RunTx(t.Context(), f.Pool, func(outer pgx.Tx) error {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				func() {
					defer func() {
						if value := recover(); value != nil && mode != "panic" {
							panic(value)
						}
					}()
					err := retry.Run(ctx, outer, request, func(tx pgx.Tx, r retry.Result) (retry.Completion, error) {
						if _, err := tx.Exec(ctx, `UPDATE computers SET key='partial' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer); err != nil {
							return retry.Completion{}, err
						}
						switch mode {
						case "error":
							return retry.Completion{}, errors.New("domain failed")
						case "panic":
							panic("domain failed")
						case "cancel":
							cancel()
							return retry.Completion{Target: retry.Target{DeploymentID: f.Deployment}, Receipt: []byte(`{}`)}, nil
						default:
							return retry.Completion{}, nil
						}
					})
					if err == nil {
						t.Error("failed mutation succeeded")
					}
				}()
				// The key remains locked by the caller after the failed savepoint.
				var locks int
				if err := outer.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND granted`).Scan(&locks); err != nil || locks != 1 {
					t.Fatalf("caller advisory locks: %d, %v", locks, err)
				}
				// Deliberately ignore the inner error and commit unrelated work.
				_, err := outer.Exec(t.Context(), `UPDATE computers SET unused_since=NULL WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			assertNoPartialMutation(t, f)
			accept(t, f, request, retry.Target{DeploymentID: f.Deployment})
		})
	}
}

func assertNoPartialMutation(t *testing.T, f agenttest.Fixture) {
	t.Helper()
	var partial bool
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT COALESCE(key='partial',false) FROM computers WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer).Scan(&partial); err != nil || partial {
		t.Fatalf("domain mutation survived: %v, %v", partial, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_receipts WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retry records survived: %d, %v", count, err)
	}
}

func TestPlatformNestedSameKeyCannotCommitTwoMutations(t *testing.T) {
	f := agenttest.New(t)
	request := deploymentRequest(t, f, "nested")
	completion := retry.Completion{Target: retry.Target{DeploymentID: f.Deployment}, Receipt: []byte(`{}`)}
	var mutationErr error
	err := db.RunTx(t.Context(), f.Pool, func(outer pgx.Tx) error {
		mutationErr = retry.Run(t.Context(), outer, request, func(tx pgx.Tx, r retry.Result) (retry.Completion, error) {
			if _, err := tx.Exec(t.Context(), `UPDATE computers SET key='partial' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer); err != nil {
				return retry.Completion{}, err
			}
			if err := retry.Run(t.Context(), tx, request, func(inner pgx.Tx, r retry.Result) (retry.Completion, error) { return completion, nil }); err != nil {
				return retry.Completion{}, err
			}
			return completion, nil
		})
		return nil // Deliberately commit the caller after the failed unit.
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutationErr == nil {
		t.Fatal("two new receipts for the same identity committed")
	}
	assertNoPartialMutation(t, f)
	accept(t, f, request, completion.Target)
}

func TestPlatformSuccessfulInnerMutationFollowsOuterRollback(t *testing.T) {
	f := agenttest.New(t)
	request := deploymentRequest(t, f, "outer-rollback")
	rollback := errors.New("outer rejected")
	err := db.RunTx(t.Context(), f.Pool, func(outer pgx.Tx) error {
		if err := retry.Run(t.Context(), outer, request, func(tx pgx.Tx, r retry.Result) (retry.Completion, error) {
			if _, err := tx.Exec(t.Context(), `UPDATE computers SET key='partial' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer); err != nil {
				return retry.Completion{}, err
			}
			return retry.Completion{Target: retry.Target{DeploymentID: f.Deployment}, Receipt: []byte(`{}`)}, nil
		}); err != nil {
			return err
		}
		var count int
		if err := outer.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_receipts WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 1 {
			t.Fatalf("inner receipt not visible: %d, %v", count, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	assertNoPartialMutation(t, f)
}
