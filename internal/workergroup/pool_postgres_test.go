package workergroup

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestCreatePoolPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	group, pending, err := CreatePool(t.Context(), f.pool, f.groupID(), "run-next", f.group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" || pending.Name != "run-next" || group.ID != f.group.ID {
		t.Fatalf("created pool = %+v in group %+v", pending, group)
	}
	var conflicting ConflictError
	if _, _, err := CreatePool(t.Context(), f.pool, f.groupID(), "run-next", f.group.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("duplicate pool error = %v, want ConflictError", err)
	}
	if _, _, err := CreatePool(t.Context(), f.pool, f.groupID(), "stale", f.group.ClaimVersion+1); !errors.As(err, &conflicting) {
		t.Fatalf("stale group claim error = %v, want ConflictError", err)
	}
	if _, _, err := CreatePool(t.Context(), f.pool, uuid.NewV7(), "missing", 1); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("missing group error = %v", err)
	}
	var input InputError
	for _, bad := range []struct {
		name    string
		version int64
	}{{"Upper", 1}, {"valid", 0}} {
		if _, _, err := CreatePool(t.Context(), f.pool, f.groupID(), bad.name, bad.version); !errors.As(err, &input) {
			t.Fatalf("CreatePool(%q, %d) error = %v, want InputError", bad.name, bad.version, err)
		}
	}
	_, pools, err := ListPools(t.Context(), f.q, f.groupID())
	if err != nil || len(pools) != 1 || pools[0].ID != pending.ID {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	if _, _, err := ListPools(t.Context(), f.q, uuid.NewV7()); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("ListPools missing group error = %v", err)
	}
}

func TestSelectPrimaryPoolPostgresIsAtomicAndReplaySafe(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "current")
	f.activeHost(t, pool, "ready-current")
	group := f.currentGroup(t)
	selection, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, group.ClaimVersion, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Applied || selection.Group.ClaimVersion != group.ClaimVersion+1 ||
		selection.Group.PrimaryPoolID != pool.ID || selection.Pool.ID != pool.ID {
		t.Fatalf("selection = %+v", selection)
	}
	stored := f.currentGroup(t)
	if stored.ClaimVersion != selection.Group.ClaimVersion || stored.PrimaryPoolID != pool.ID {
		t.Fatalf("stored primary selection = %+v", stored)
	}
	replay, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, group.ClaimVersion, 1)
	if err != nil || replay.Applied || replay.Group.ClaimVersion != selection.Group.ClaimVersion {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	var conflicting ConflictError
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, selection.Group.ClaimVersion+1, 1); !errors.As(err, &conflicting) {
		t.Fatalf("future claim error = %v, want ConflictError", err)
	}
	pending := f.pendingPool(t, "pending")
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, f.currentGroup(t).ClaimVersion, 1); !errors.As(err, &conflicting) {
		t.Fatalf("pending pool selection error = %v, want ConflictError", err)
	}
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), uuid.NewV7(), 1, 1); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("missing pool error = %v", err)
	}
	var input InputError
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), uuid.Nil(), 1, 1); !errors.As(err, &input) {
		t.Fatalf("zero pool error = %v, want InputError", err)
	}
	if _, err := SelectPrimaryPool(t.Context(), f.pool, uuid.NewV7(), uuid.Nil(), 1, 1); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("zero pool on missing group error = %v, want ErrGroupNotFound first", err)
	}
}

func TestSelectPrimaryPoolPostgresSerializesCompetingControllers(t *testing.T) {
	f := newSupplyFixture(t)
	first := f.activePool(t, "first")
	second := f.activePool(t, "second")
	f.activeHost(t, first, "ready-first")
	f.activeHost(t, second, "ready-second")
	group := f.currentGroup(t)
	targets := []uuid.UUID{first.ID.Bytes, second.ID.Bytes}
	start := make(chan struct{})
	errorsByController := make([]error, len(targets))
	var wait sync.WaitGroup
	for index, target := range targets {
		wait.Go(func() {
			<-start
			_, errorsByController[index] = SelectPrimaryPool(t.Context(), f.pool, f.groupID(), target, group.ClaimVersion, 1)
		})
	}
	close(start)
	wait.Wait()
	succeeded := 0
	for _, err := range errorsByController {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("controller errors = %v, want exactly one success", errorsByController)
	}
	stored := f.currentGroup(t)
	firstWon := stored.PrimaryPoolID == first.ID
	secondWon := stored.PrimaryPoolID == second.ID
	if stored.ClaimVersion != group.ClaimVersion+1 || firstWon == secondWon {
		t.Fatalf("stored competing primary selection = %+v", stored)
	}
}

func TestPoolTransitionsPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	pending := f.pendingPool(t, "unused")
	var conflicting ConflictError
	if _, _, err := DrainPool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("drain pending pool error = %v, want ConflictError", err)
	}
	_, disabled, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
		t.Fatalf("disabled pending pool = %+v", disabled)
	}
	_, replay, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion)
	if err != nil || replay.ClaimVersion != disabled.ClaimVersion {
		t.Fatalf("disable replay = %+v, %v", replay, err)
	}
	if _, _, err := DisablePool(t.Context(), f.pool, f.groupID(), uuid.NewV7(), 1); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("missing pool error = %v", err)
	}
	var input InputError
	if _, _, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, 0); !errors.As(err, &input) {
		t.Fatalf("zero claim version error = %v, want InputError", err)
	}
}

func TestDisablePoolPostgresWaitsForRegisteringHostToBeLost(t *testing.T) {
	f := newSupplyFixture(t)
	pending := f.pendingPool(t, "lost-before-activation")
	hostID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `
INSERT INTO worker_hosts (id, resource_id, worker_group_id, worker_pool_id, status)
VALUES ($1, 'lost-before-activation', $2, $3, 'registering')`, hostID, f.group.ID, pending.ID)
	var conflicting ConflictError
	if _, _, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("disable pending pool with registering host error = %v, want ConflictError", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status = 'lost', lost_at = now() WHERE id = $1`, hostID)
	_, disabled, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
		t.Fatalf("disabled pending pool with lost host = %+v", disabled)
	}
}

func TestDrainPoolPostgresRejectsPrimaryPool(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "current")
	f.activeHost(t, pool, "ready-current")
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, f.currentGroup(t).ClaimVersion, 1); err != nil {
		t.Fatal(err)
	}
	var conflicting ConflictError
	if _, _, err := DrainPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, pool.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("drain primary pool error = %v, want ConflictError", err)
	}
	if got := f.currentGroup(t); got.Status != db.WorkerGroupStatusActive {
		t.Fatalf("group = %+v", got)
	}
}

func TestSelectPrimaryPoolPostgresRequiresReadyHosts(t *testing.T) {
	for name, mutation := range map[string]string{
		"unobserved": "observed_at=NULL", "stale": "observed_at=now()-interval '3 minutes'",
		"paused": "run_paused_reason='test'", "VM paused": "vm_paused_reason='test'",

		"draining": "status='draining',draining_at=now(),drain_reason='replacement'",
	} {
		t.Run(name, func(t *testing.T) {
			f := newSupplyFixture(t)
			pool := f.activePool(t, "target")
			host := f.activeHost(t, pool, "ready")
			dbtest.MustExec(t, t.Context(), f.pool, "UPDATE worker_hosts SET "+mutation+" WHERE id=$1", host)
			if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, f.currentGroup(t).ClaimVersion, 1); !errors.Is(err, ErrInsufficientReadyHosts) {
				t.Fatalf("ineligible host selected: %v", err)
			}
		})
	}
	f := newSupplyFixture(t)
	pool := f.activePool(t, "target")
	f.activeHost(t, pool, "ready")
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, f.currentGroup(t).ClaimVersion, 2); !errors.Is(err, ErrInsufficientReadyHosts) {
		t.Fatalf("minimum ignored: %v", err)
	}
}

func TestSelectPrimaryPoolPostgresRechecksAfterConcurrentDrain(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "target")
	hostID := f.activeHost(t, pool, "ready")
	group := f.currentGroup(t)
	hold := f.begin(t)
	locked, err := LockHostWithPool(t.Context(), db.New(hold), f.groupID(), pool.ID.Bytes, hostID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var blocker int32
	if err = hold.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := SelectPrimaryPool(ctx, f.pool, f.groupID(), pool.ID.Bytes, group.ClaimVersion, 1)
		done <- err
	}()
	for {
		var blocked bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("selection did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	_, err = db.New(hold).DrainWorkerHost(ctx, db.DrainWorkerHostParams{ID: pgvalue.UUID(hostID), WorkerGroupID: group.ID, ExpectedEpoch: locked.Host.CurrentEpoch, ExpectedClaimVersion: locked.Host.ClaimVersion, DrainReason: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrInsufficientReadyHosts) {
		t.Fatalf("drained target selected: %v", err)
	}
	if f.currentGroup(t).PrimaryPoolID.Valid {
		t.Fatal("primary changed despite concurrent drain")
	}
}

func TestPoolRetirementPostgresFencesWaitingEnrollment(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "retiring")
	cfg := testHostAuthConfig(t)
	token, err := auth.ParseEnrollmentToken(f.enrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	hold := f.begin(t)
	q := db.New(hold)
	if _, err = q.LockWorkerGroupForPoolMutation(t.Context(), f.group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = q.LockWorkerPool(t.Context(), db.LockWorkerPoolParams{WorkerGroupID: f.group.ID, WorkerPoolID: pool.ID}); err != nil {
		t.Fatal(err)
	}
	var blocker int32
	if err = hold.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := EnrollHost(ctx, f.pool, cfg, Enrollment{TokenHash: token, PoolName: pool.Name, ResourceID: "late"})
		done <- err
	}()
	for {
		var blocked bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("enrollment did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err = q.TransitionWorkerPoolLifecycle(ctx, db.TransitionWorkerPoolLifecycleParams{TargetStatus: "draining", WorkerPoolID: pool.ID, WorkerGroupID: f.group.ID, ExpectedPoolClaimVersion: pool.ClaimVersion}); err != nil {
		t.Fatal(err)
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("enrollment entered withdrawn pool: %v", err)
	}
	var hosts int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM worker_hosts WHERE worker_pool_id=$1`, pool.ID).Scan(&hosts); err != nil || hosts != 0 {
		t.Fatalf("hosts=%d: %v", hosts, err)
	}
}

func TestSelectPrimaryPoolPostgresFreshnessAfterHostLock(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "target")
	hostID := f.activeHost(t, pool, "ready")
	group := f.currentGroup(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '119 seconds' WHERE id=$1`, hostID)
	hold := f.begin(t)
	dbtest.MustExec(t, t.Context(), hold, `SELECT id FROM worker_hosts WHERE id=$1 FOR UPDATE`, hostID)
	var blocker int32
	if err := hold.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := SelectPrimaryPool(ctx, f.pool, f.groupID(), pool.ID.Bytes, group.ClaimVersion, 1)
		done <- err
	}()
	for {
		var blocked, expired bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))),observed_at<clock_timestamp()-interval '120 seconds' FROM worker_hosts WHERE id=$2`, blocker, hostID).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("selection did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// No row changed while held: freshness must be evaluated again even without
	// PostgreSQL's concurrent-update predicate recheck.
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInsufficientReadyHosts) {
		t.Fatalf("expired observation selected: %v", err)
	}
}
