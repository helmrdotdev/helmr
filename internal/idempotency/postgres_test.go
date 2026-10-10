package idempotency_test

import (
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

func deploymentRequest(t *testing.T, f agenttest.Fixture, key string) retry.Request {
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
func accept(t *testing.T, f agenttest.Fixture, request retry.Request, target retry.Target) db.PlatformRetryKey {
	t.Helper()
	var claim db.PlatformRetryKey
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		claims, _ := retry.TransactionFor(tx)
		r, err := claims.Acquire(t.Context(), request)
		if err != nil {
			return err
		}
		claim = r.Claim
		if !r.New {
			return nil
		}
		claim, err = claims.Complete(t.Context(), r.Claim, target, []byte(`{"accepted":true}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return claim
}
func acquireError(t *testing.T, f agenttest.Fixture, request retry.Request) error {
	t.Helper()
	return db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		claims, _ := retry.TransactionFor(tx)
		_, err := claims.Acquire(t.Context(), request)
		return err
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
			err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				claims, _ := retry.TransactionFor(tx)
				var err error
				r, err = claims.Acquire(t.Context(), request)
				if err != nil {
					return err
				}
				if r.New {
					r.Claim, err = claims.Complete(t.Context(), r.Claim, retry.Target{DeploymentID: f.Deployment}, []byte(`{"accepted":true}`))
				}
				return err
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
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM platform_retry_keys WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts %d: %v", count, err)
	}
}
func TestPlatformIncompleteTransactionAndWrongTargetRollBack(t *testing.T) {
	for _, mode := range []string{"unfinished", "wrong-family", "foreign-target"} {
		t.Run(mode, func(t *testing.T) {
			f := agenttest.New(t)
			request := deploymentRequest(t, f, mode)
			err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				claims, _ := retry.TransactionFor(tx)
				r, err := claims.Acquire(t.Context(), request)
				if err != nil {
					return err
				}
				if mode == "unfinished" {
					return nil
				}
				target := retry.Target{ComputerID: f.Computer}
				if mode == "foreign-target" {
					target = retry.Target{DeploymentID: uuid.NewV7()}
				}
				_, err = claims.Complete(t.Context(), r.Claim, target, []byte(`{}`))
				return err
			})
			if err == nil {
				t.Fatal("incomplete or mistyped result committed")
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM platform_retry_keys WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial claim retained %d: %v", count, err)
			}
			accept(t, f, request, retry.Target{DeploymentID: f.Deployment})
		})
	}
}
func TestPlatformExpiredIdentityCannotExecuteBeforeOrAfterCollection(t *testing.T) {
	f := agenttest.New(t)
	request := deploymentRequest(t, f, "expired")
	claim := accept(t, f, request, retry.Target{DeploymentID: f.Deployment})
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE platform_retry_keys SET accepted_at=accepted_at-interval '31 days',receipt_expires_at=receipt_expires_at-interval '31 days' WHERE environment_id=$1 AND id=$2`, f.Environment, claim.ID)
	var expired retry.ExpiredError
	if err := acquireError(t, f, request); !errors.As(err, &expired) {
		t.Fatalf("uncollected expiry: %v", err)
	}
	if count, err := db.New(f.Pool).PruneExpiredPlatformRetryReceipts(t.Context(), 100); err != nil || count != 1 {
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
