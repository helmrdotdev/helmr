package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func renewalFixture(t *testing.T) (*Server, runtest.Fixture, runtest.RunLease, workergroup.HostPrincipal, workerapi.RunLeaseFence, time.Time) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now(),max_active_duration_ms=3600000 WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 millisecond',expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, work.LeaseID)
	var expiry time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	fence := workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1}
	return &Server{tx: f.Pool}, f, work, worker, fence, expiry
}

func TestRenewRunLeaseRetainsOnlyPreviousReceiptAndProjectsAttemptBase(t *testing.T) {
	s, f, work, w, fence, expiry := renewalFixture(t)
	first, err := s.renewRunLease(t.Context(), w, pgvalue.UUID(work.LeaseID), fence, expiry)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.renewRunLease(t.Context(), w, pgvalue.UUID(work.LeaseID), fence, first.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) {
		t.Fatal("second renewal did not advance")
	}
	replay, err := s.renewRunLease(t.Context(), w, pgvalue.UUID(work.LeaseID), fence, first.ExpiresAt)
	if err != nil || !replay.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatalf("last receipt replay: %+v %v", replay, err)
	}
	if _, err = s.renewRunLease(t.Context(), w, pgvalue.UUID(work.LeaseID), fence, expiry); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("two-renewals-old receipt accepted: %v", err)
	}
	var base string
	if err = f.Pool.QueryRow(t.Context(), `SELECT base_computer_disk_version_id::text FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if second.Lease != fence || second.BaseComputerDiskVersionID != base {
		t.Fatalf("renewal projection changed logical fence/base: %+v", second)
	}
}

func TestRenewRunLeaseDoesNotWriteWhenHorizonDoesNotAdvance(t *testing.T) {
	s, f, work, w, fence, _ := renewalFixture(t)
	var expiry time.Time
	if err := f.Pool.QueryRow(t.Context(), `UPDATE run_leases SET expires_at=clock_timestamp()+interval '10 minutes' WHERE id=$1 RETURNING expires_at`, work.LeaseID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	renewed, err := s.renewRunLease(t.Context(), w, pgvalue.UUID(work.LeaseID), fence, expiry)
	if err != nil || !renewed.ExpiresAt.Equal(expiry) {
		t.Fatalf("no-extension=%+v %v", renewed, err)
	}
	var same bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text=$2 FROM run_leases l WHERE id=$1`, work.LeaseID, before).Scan(&same); err != nil || !same {
		t.Fatalf("no-extension wrote lease: %v %v", same, err)
	}
}

func TestRenewRunLeaseAllowsDrainingOwner(t *testing.T) {
	s, f, work, w, fence, expiry := renewalFixture(t)
	drained, err := db.New(f.Pool).DrainWorkerHost(t.Context(), db.DrainWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.HostClaimVersion = drained.ClaimVersion
	renewed, err := s.renewRunLease(t.Context(), w, pgvalue.UUID(work.LeaseID), fence, expiry)
	if err != nil || !renewed.ExpiresAt.After(expiry) {
		t.Fatalf("draining renewal=%+v %v", renewed, err)
	}
}

func TestRenewRunLeaseRejectsStaleAuthorityWithoutWriting(t *testing.T) {
	for _, kind := range []string{"sequence", "epoch", "elapsed budget", "active deadline"} {
		t.Run(kind, func(t *testing.T) {
			s, f, work, w, fence, expiry := renewalFixture(t)
			switch kind {
			case "sequence":
				fence.LeaseSequence++
			case "epoch":
				w.Epoch++
			case "elapsed budget":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_elapsed_ms=max_active_duration_ms WHERE id=$1`, work.RunID)
			case "active deadline":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, work.RunID)
			}
			var before string
			if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := s.renewRunLease(t.Context(), w, pgvalue.UUID(work.LeaseID), fence, expiry); !errors.Is(err, errStaleRunLeaseClaim) {
				t.Fatalf("stale renewal=%v", err)
			}
			var same bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text=$2 FROM run_leases l WHERE id=$1`, work.LeaseID, before).Scan(&same); err != nil || !same {
				t.Fatalf("rejected renewal wrote lease: %v %v", same, err)
			}
		})
	}
}

func TestRenewRunLeaseRejectsExpiryDuringComputerLockWait(t *testing.T) {
	s, f, work, w, fence, expiry := renewalFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var pid int32
	if err = blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, blocker, `SELECT id FROM computers WHERE id=(SELECT computer_id FROM runs WHERE id=$1) FOR UPDATE`, work.RunID)
	done := make(chan error, 1)
	go func() { _, e := s.renewRunLease(ctx, w, pgvalue.UUID(work.LeaseID), fence, expiry); done <- e }()
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a WHERE $1=ANY(pg_blocking_pids(a.pid))),expires_at<clock_timestamp() FROM run_leases WHERE id=$2`, pid, work.LeaseID).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("renewal did not wait: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, errStaleRunLeaseClaim) {
			t.Fatalf("expired renewal=%v", e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var untouched bool
	if err = f.Pool.QueryRow(ctx, `SELECT expires_at=$2 AND previous_expires_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID, expiry).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("expired renewal changed receipt: %v %v", untouched, err)
	}
}
