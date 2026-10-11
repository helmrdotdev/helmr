package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerCapacityBinsReturnBoundedHostPrefix(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	seedCapacityQueryWorkers(t, ctx, pool, 1002, 1001)
	q := db.New(pool)
	bins, err := q.ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		RowLimit: 1001,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) != 1001 {
		t.Fatalf("planner Worker prefix has %d rows, want 1001", len(bins))
	}
	if bins[len(bins)-1].WorkerHostID != capacityQueryWorkerID(1001) {
		t.Fatalf("planner Worker prefix ends at %s", pgvalue.UUIDString(bins[len(bins)-1].WorkerHostID))
	}

}

func seedCapacityQueryWorkers(t *testing.T, ctx context.Context, pool *pgxpool.Pool, count, paused int) {
	t.Helper()
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO worker_hosts (
    id, resource_id, worker_group_id, worker_pool_id, status,
    current_epoch, current_service_id, vm_platform_id,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, max_vm_starts, cpu_environment, cpu_environment_digest,
    run_paused_reason, observed_at, epoch_started_at, activated_at
)
SELECT ('00000000-0000-7000-8000-' || lpad(value::text, 12, '0'))::uuid,
       'capacity-worker-' || value,
       $1, $2, 'active', 1,
       ('10000000-0000-7000-8000-' || lpad(value::text, 12, '0'))::uuid,
       $3,
       8000, 17179869184, 274877906944,
       4000, 8589934592, 34359738368,
       8, 8, '{}'::jsonb, $4,
       CASE WHEN value <= $5 THEN 'test-incompatible' ELSE NULL END,
       now(), now(), now()
  FROM generate_series(1, $6::integer) AS value`,
		dbtest.DefaultWorkerGroupID, dbtest.DefaultWorkerPoolID, dbtest.DefaultRuntimeID,
		dbtest.DefaultCPUConfigID, paused, count,
	)
}

func capacityQueryWorkerID(value int) pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8000-%012d", value)))
}

func TestWorkerCredentialExchangePreservesRuntimeLockOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID, serviceID := uuid.NewV7(), uuid.NewV7()
	secretHash := []byte("credential-lock-order-secret")
	enrollTestWorker(t, ctx, q, workerID, "credential-lock-order-worker", secretHash)
	params := db.AuthenticateWorkerHostSecretParams{
		WorkerHostID: pgvalue.UUID(workerID), SecretHash: secretHash, ServiceID: pgvalue.UUID(serviceID),
	}
	first, err := q.AuthenticateWorkerHostSecret(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ActivateWorkerHost(ctx, testWorkerActivationParams(workerID, first.CurrentEpoch)); err != nil {
		t.Fatal(err)
	}
	runtime, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Rollback(context.Background())
	var groupClaim int64
	var groupStatus string
	if err := runtime.QueryRow(ctx, `SELECT claim_version,status FROM worker_groups WHERE id=$1 FOR SHARE`, dbtest.DefaultWorkerGroupID).Scan(&groupClaim, &groupStatus); err != nil {
		t.Fatal(err)
	}
	exchange, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		row db.AuthenticateWorkerHostSecretRow
		err error
	}
	exchangePID, runtimePID := exchange.Conn().PgConn().PID(), runtime.Conn().PgConn().PID()
	result := make(chan outcome, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		row, err := db.New(exchange).AuthenticateWorkerHostSecret(ctx, params)
		result <- outcome{row, err}
	}()
	defer func() {
		cancel()
		<-done
		exchange.Release()
	}()
	// Wait for the exchange to reach our group lock, rather than guessing when
	// its statement starts. It must not hold the host while waiting for the group.
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT $2::integer = ANY(pg_blocking_pids($1))`, exchangePID, runtimePID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case value := <-result:
			t.Fatalf("credential exchange did not wait for runtime admission: %v", value.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	var hostClaim int64
	var epoch pgtype.Int8
	var hostStatus string
	if err := runtime.QueryRow(ctx, `SELECT claim_version,current_epoch,status FROM worker_hosts WHERE id=$1 AND worker_group_id=$2 FOR SHARE NOWAIT`, workerID, dbtest.DefaultWorkerGroupID).Scan(&hostClaim, &epoch, &hostStatus); err != nil {
		t.Fatalf("runtime admission blocked behind credential exchange: %v", err)
	}
	if epoch != first.CurrentEpoch || hostStatus != "active" || hostClaim != first.ClaimVersion || groupClaim != first.GroupClaimVersion || groupStatus != "active" {
		t.Fatal("runtime authority changed before credential exchange")
	}
	if err := runtime.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	value := <-result
	if value.err != nil {
		t.Fatal(value.err)
	}
	if value.row.CurrentEpoch != first.CurrentEpoch || value.row.CurrentServiceID != first.CurrentServiceID || value.row.Status != db.WorkerHostStatusActive {
		t.Fatalf("same-service exchange changed active epoch: %+v", value.row)
	}
}

func TestWorkerEpochOwnsLivenessAndActivationReplayPreservesIt(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.NewV7()
	serviceID := uuid.NewV7()
	secretHash := []byte("epoch-liveness-secret")
	enrollTestWorker(t, ctx, q, workerID, "epoch-liveness-worker", secretHash)

	authenticate := func(service uuid.UUID) db.AuthenticateWorkerHostSecretRow {
		t.Helper()
		row, err := q.AuthenticateWorkerHostSecret(ctx, db.AuthenticateWorkerHostSecretParams{
			WorkerHostID: pgvalue.UUID(workerID), SecretHash: secretHash,
			ServiceID: pgvalue.UUID(service),
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	firstEpoch := authenticate(serviceID)
	activationParams := testWorkerActivationParams(workerID, firstEpoch.CurrentEpoch)
	activated, err := q.ActivateWorkerHost(ctx, activationParams)
	if err != nil {
		t.Fatal(err)
	}
	if activated.ObservedAt.Valid || activated.RunPausedReason.Valid || activated.VMPausedReason.Valid {
		t.Fatalf("initial activation liveness = observed:%+v run:%+v vm:%+v", activated.ObservedAt, activated.RunPausedReason, activated.VMPausedReason)
	}
	bins, err := q.ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		RowLimit:                    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, bin := range bins {
		if bin.WorkerHostID == pgvalue.UUID(workerID) {
			t.Fatalf("unobserved active worker appeared in capacity bins: %+v", bin)
		}
	}
	authorization := db.AuthorizeActivatingWorkerHostSecretParams{
		HostSecretID: firstEpoch.ID, ClaimVersion: firstEpoch.ClaimVersion,
		GroupClaimVersion: firstEpoch.GroupClaimVersion, WorkerEpoch: firstEpoch.CurrentEpoch,
	}
	if authorized, err := q.AuthorizeActivatingWorkerHostSecret(ctx, authorization); err != nil {
		t.Fatal(err)
	} else if authorized.WorkerStatus != db.WorkerHostStatusActive {
		t.Fatalf("activation replay authorization state = %q, want active", authorized.WorkerStatus)
	}
	staleAuthorization := authorization
	staleAuthorization.WorkerEpoch.Int64++
	if _, err := q.AuthorizeActivatingWorkerHostSecret(ctx, staleAuthorization); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale activation authorization error = %v, want pgx.ErrNoRows", err)
	}
	changed := activationParams
	changed.MaxVMSlots++
	if _, err := q.ActivateWorkerHost(ctx, changed); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("changed activation replay error = %v, want pgx.ErrNoRows", err)
	}

	sentinel := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_hosts
		   SET observed_at = $2,
		       run_paused_reason = NULL,
		       vm_paused_reason = 'startup_recovery_leak'
		 WHERE id = $1
	`, workerID, sentinel); err != nil {
		t.Fatal(err)
	}
	replayed, err := q.ActivateWorkerHost(ctx, activationParams)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.ObservedAt.Valid || !replayed.ObservedAt.Time.Equal(sentinel) || replayed.RunPausedReason.Valid ||
		!replayed.VMPausedReason.Valid || replayed.VMPausedReason.String != "startup_recovery_leak" {
		t.Fatalf("activation replay changed liveness = observed:%+v run:%+v vm:%+v", replayed.ObservedAt, replayed.RunPausedReason, replayed.VMPausedReason)
	}

	sameEpoch := authenticate(serviceID)
	if sameEpoch.CurrentEpoch != firstEpoch.CurrentEpoch {
		t.Fatalf("same service epoch = %+v, want %+v", sameEpoch.CurrentEpoch, firstEpoch.CurrentEpoch)
	}
	var preservedObservedAt pgtype.Timestamptz
	var preservedVMPause pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT observed_at, vm_paused_reason FROM worker_hosts WHERE id = $1`, workerID).Scan(&preservedObservedAt, &preservedVMPause); err != nil {
		t.Fatal(err)
	}
	if !preservedObservedAt.Valid || !preservedObservedAt.Time.Equal(sentinel) || preservedVMPause.String != "startup_recovery_leak" {
		t.Fatalf("same service changed liveness = observed:%+v vm:%+v", preservedObservedAt, preservedVMPause)
	}

	nextEpoch := authenticate(uuid.NewV7())
	if !nextEpoch.CurrentEpoch.Valid || nextEpoch.CurrentEpoch.Int64 != firstEpoch.CurrentEpoch.Int64+1 || nextEpoch.Status != db.WorkerHostStatusRegistering {
		t.Fatalf("new service epoch = %+v", nextEpoch)
	}
	var observedAt pgtype.Timestamptz
	var runPause, vmPause pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT observed_at, run_paused_reason, vm_paused_reason FROM worker_hosts WHERE id = $1`, workerID).Scan(&observedAt, &runPause, &vmPause); err != nil {
		t.Fatal(err)
	}
	if observedAt.Valid || runPause.Valid || vmPause.Valid {
		t.Fatalf("new epoch retained liveness = observed:%+v run:%+v vm:%+v", observedAt, runPause, vmPause)
	}
}

func TestDrainingWorkerActivationSurvivesRestartAndLostResponse(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.NewV7()
	secretHash := []byte("draining-restart-secret")
	enrollTestWorker(t, ctx, q, workerID, "draining-restart-worker", secretHash)
	authenticate := func(serviceID uuid.UUID) db.AuthenticateWorkerHostSecretRow {
		t.Helper()
		row, err := q.AuthenticateWorkerHostSecret(ctx, db.AuthenticateWorkerHostSecretParams{
			WorkerHostID: pgvalue.UUID(workerID),
			SecretHash:   secretHash,
			ServiceID:    pgvalue.UUID(serviceID),
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	authorizeActivation := func(row db.AuthenticateWorkerHostSecretRow) db.AuthorizeActivatingWorkerHostSecretRow {
		t.Helper()
		authorized, err := q.AuthorizeActivatingWorkerHostSecret(ctx, db.AuthorizeActivatingWorkerHostSecretParams{
			HostSecretID:      row.ID,
			ClaimVersion:      row.ClaimVersion,
			GroupClaimVersion: row.GroupClaimVersion,
			WorkerEpoch:       row.CurrentEpoch,
		})
		if err != nil {
			t.Fatal(err)
		}
		return authorized
	}

	firstServiceID := uuid.NewV7()
	firstEpoch := authenticate(firstServiceID)
	firstActivation := testWorkerActivationParams(workerID, firstEpoch.CurrentEpoch)
	active, err := q.ActivateWorkerHost(ctx, firstActivation)
	if err != nil {
		t.Fatal(err)
	}
	draining, err := q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{DrainReason: "shutdown",
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        firstEpoch.CurrentEpoch,
		ExpectedClaimVersion: active.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.Status != db.WorkerHostStatusDraining || !draining.DrainingAt.Valid {
		t.Fatalf("draining worker = %+v", draining)
	}

	sameEpoch := authenticate(firstServiceID)
	if authorized := authorizeActivation(sameEpoch); authorized.WorkerStatus != db.WorkerHostStatusDraining {
		t.Fatalf("draining activation replay authorization = %+v", authorized)
	}
	replayedDraining, err := q.ActivateWorkerHost(ctx, firstActivation)
	if err != nil {
		t.Fatal(err)
	}
	if replayedDraining.Status != db.WorkerHostStatusDraining || replayedDraining.DrainingAt != draining.DrainingAt {
		t.Fatalf("draining activation replay = %+v, want draining at %+v", replayedDraining, draining.DrainingAt)
	}
	mismatched := firstActivation
	mismatched.MaxVMSlots++
	if _, err := q.ActivateWorkerHost(ctx, mismatched); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched draining activation error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := q.LockWorkerHostForActivation(ctx, db.LockWorkerHostForActivationParams{
		WorkerHostID:  pgvalue.UUID(workerID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		WorkerPoolID:  pgvalue.NewUUIDv7(),
		WorkerEpoch:   firstEpoch.CurrentEpoch,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched draining pool fence error = %v, want pgx.ErrNoRows", err)
	}

	nextEpoch := authenticate(uuid.NewV7())
	if nextEpoch.Status != db.WorkerHostStatusDraining ||
		nextEpoch.CurrentEpoch.Int64 != firstEpoch.CurrentEpoch.Int64+1 {
		t.Fatalf("restarted draining epoch = %+v", nextEpoch)
	}
	cleared := authorizeActivation(nextEpoch)
	if cleared.WorkerStatus != db.WorkerHostStatusDraining {
		t.Fatalf("restarted draining authorization = %+v", cleared)
	}
	staleAuthorization := db.AuthorizeActivatingWorkerHostSecretParams{
		HostSecretID:      nextEpoch.ID,
		ClaimVersion:      nextEpoch.ClaimVersion,
		GroupClaimVersion: nextEpoch.GroupClaimVersion,
		WorkerEpoch:       firstEpoch.CurrentEpoch,
	}
	if _, err := q.AuthorizeActivatingWorkerHostSecret(ctx, staleAuthorization); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale draining epoch authorization error = %v, want pgx.ErrNoRows", err)
	}

	restartedActivation := testWorkerActivationParams(workerID, nextEpoch.CurrentEpoch)
	restarted, err := q.ActivateWorkerHost(ctx, restartedActivation)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Status != db.WorkerHostStatusDraining || restarted.DrainingAt != draining.DrainingAt {
		t.Fatalf("restarted draining activation = %+v", restarted)
	}
	lostResponseReplay, err := q.ActivateWorkerHost(ctx, restartedActivation)
	if err != nil {
		t.Fatal(err)
	}
	if lostResponseReplay.Status != db.WorkerHostStatusDraining ||
		lostResponseReplay.CurrentEpoch != nextEpoch.CurrentEpoch ||
		lostResponseReplay.DrainingAt != restarted.DrainingAt {
		t.Fatalf("lost activation response replay = %+v, want %+v", lostResponseReplay, restarted)
	}
}

func testWorkerActivationParams(workerID uuid.UUID, epoch pgtype.Int8) db.ActivateWorkerHostParams {
	return db.ActivateWorkerHostParams{
		WorkerHostID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID, WorkerEpoch: epoch,
		EpochCPUMillis: 2000, EpochMemoryBytes: 2 << 30, EpochGuestEphemeralDiskBytes: 64 << 30,
		MaxVMSlots: 1, VMPlatformID: pgtype.Text{String: dbtest.DefaultRuntimeID, Valid: true},
		PerVMCPUMillis: 1000, PerVMMemoryBytes: 1 << 30, PerVMGuestEphemeralDiskBytes: 32 << 30,
		MaxVMStarts:          1,
		CPUEnvironment:       []byte(`{}`),
		CPUEnvironmentDigest: pgtype.Text{String: dbtest.DefaultCPUConfigID, Valid: true},
	}
}

func TestWorkerGroupStatusTransitionsAreFencedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	q := db.New(newPostgresDB(t, ctx))
	initial, err := q.GetWorkerGroupStatus(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := q.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetStatus: string(db.WorkerGroupStatusPaused),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != db.WorkerGroupStatusPaused || paused.ClaimVersion != initial.ClaimVersion+1 || !paused.TransitionApplied {
		t.Fatalf("paused = %+v", paused)
	}
	replayed, err := q.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetStatus: string(db.WorkerGroupStatusPaused),
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != paused.ClaimVersion || replayed.TransitionApplied {
		t.Fatalf("replayed pause = %+v", replayed)
	}
	active, err := q.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetStatus: string(db.WorkerGroupStatusActive),
		ExpectedClaimVersion: paused.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != db.WorkerGroupStatusActive || active.ClaimVersion != paused.ClaimVersion+1 || !active.TransitionApplied {
		t.Fatalf("active = %+v", active)
	}
	draining, err := q.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetStatus: string(db.WorkerGroupStatusDraining),
		ExpectedClaimVersion: active.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.Status != db.WorkerGroupStatusDraining || draining.ClaimVersion != active.ClaimVersion+1 || !draining.TransitionApplied {
		t.Fatalf("draining = %+v", draining)
	}
	if _, err := q.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetStatus: string(db.WorkerGroupStatusActive),
		ExpectedClaimVersion: initial.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale reactivation error = %v", err)
	}
	drainingPool, err := q.TransitionWorkerPoolLifecycle(ctx, db.TransitionWorkerPoolLifecycleParams{
		TargetStatus:             "draining",
		WorkerPoolID:             pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)),
		WorkerGroupID:            dbtest.DefaultWorkerGroupID,
		ExpectedPoolClaimVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if drainingPool.Status != "draining" || drainingPool.ClaimVersion != 2 {
		t.Fatalf("draining Pool = %+v", drainingPool)
	}
	disabledPool, err := q.TransitionWorkerPoolLifecycle(ctx, db.TransitionWorkerPoolLifecycleParams{
		TargetStatus:             "disabled",
		WorkerPoolID:             pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)),
		WorkerGroupID:            dbtest.DefaultWorkerGroupID,
		ExpectedPoolClaimVersion: drainingPool.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabledPool.Status != "disabled" || disabledPool.ClaimVersion != drainingPool.ClaimVersion+1 {
		t.Fatalf("disabled Pool = %+v", disabledPool)
	}
	disabled, err := q.TransitionWorkerGroupStatus(ctx, db.TransitionWorkerGroupStatusParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetStatus: string(db.WorkerGroupStatusDisabled),
		ExpectedClaimVersion: draining.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != db.WorkerGroupStatusDisabled || disabled.ClaimVersion != draining.ClaimVersion+1 || !disabled.TransitionApplied {
		t.Fatalf("disabled = %+v", disabled)
	}
}

func TestDeploymentWorkerHostLossIsFencedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now())
	resourceID := "active-" + workerID.String()
	initial, err := q.GetWorkerHostStatusByResource(ctx, db.GetWorkerHostStatusByResourceParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	hostSecretID := uuid.NewV7()
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_host_secrets (
			id, worker_group_id, worker_host_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, hostSecretID, dbtest.DefaultWorkerGroupID, workerID, uuid.New().String(), initial.ClaimVersion, []byte("loss-secret")); err != nil {
		t.Fatal(err)
	}
	lost, err := q.MarkWorkerHostLost(ctx, db.MarkWorkerHostLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lost.ID != pgvalue.UUID(workerID) || lost.Status != db.WorkerHostStatusLost || lost.ClaimVersion != initial.ClaimVersion+1 || !lost.TransitionApplied {
		t.Fatalf("lost = %+v", lost)
	}
	replayed, err := q.MarkWorkerHostLost(ctx, db.MarkWorkerHostLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != lost.ClaimVersion || replayed.TransitionApplied {
		t.Fatalf("replayed loss = %+v", replayed)
	}
	if _, err := q.MarkWorkerHostLost(ctx, db.MarkWorkerHostLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: lost.ClaimVersion,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new loss error = %v", err)
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM worker_host_secrets WHERE id = $1`, hostSecretID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("worker loss did not revoke the Worker credential")
	}
}

func TestDeploymentWorkerHostLossTerminallyFencesRegisteringIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.NewV7()
	resourceID := "registering-lost-" + workerID.String()
	secretHash := []byte("registering-lost-secret")
	credential := enrollTestWorker(t, ctx, q, workerID, resourceID, secretHash)
	initial, err := q.GetWorkerHostStatusByResource(ctx, db.GetWorkerHostStatusByResourceParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Status != db.WorkerHostStatusRegistering || initial.CurrentEpoch.Valid {
		t.Fatalf("initial lifecycle = %+v, want pre-epoch registering", initial)
	}
	lost, err := q.MarkWorkerHostLost(ctx, db.MarkWorkerHostLostParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, ResourceID: resourceID,
		ExpectedClaimVersion: initial.ClaimVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lost.Status != db.WorkerHostStatusLost || lost.CurrentEpoch.Valid || lost.ClaimVersion != initial.ClaimVersion+1 {
		t.Fatalf("lost lifecycle = %+v, want terminal pre-epoch fence", lost)
	}
	if _, err := q.AuthenticateWorkerHostSecret(ctx, db.AuthenticateWorkerHostSecretParams{
		WorkerHostID: credential.WorkerHostID, SecretHash: secretHash,
		ServiceID: pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lost registering credential authentication error = %v, want pgx.ErrNoRows", err)
	}
	replacementID := uuid.NewV7()
	replacement, err := q.EnrollWorkerHost(ctx, enrollmentParams(
		replacementID, resourceID, []byte("replacement-secret"),
	))
	if err != nil {
		t.Fatalf("enroll replacement identity: %v", err)
	}
	if replacement.WorkerHostID.Bytes != replacementID || replacement.WorkerHostID.Bytes == credential.WorkerHostID.Bytes {
		t.Fatalf("replacement Worker instance ID = %v, want new ID %s", replacement.WorkerHostID, replacementID)
	}
	var lostCount, registeringCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'lost'),
		       count(*) FILTER (WHERE status = 'registering')
		  FROM worker_hosts
		 WHERE worker_group_id = $1 AND resource_id = $2
	`, dbtest.DefaultWorkerGroupID, resourceID).Scan(&lostCount, &registeringCount); err != nil {
		t.Fatal(err)
	}
	if lostCount != 1 || registeringCount != 1 {
		t.Fatalf("locator identities = lost %d registering %d, want 1 and 1", lostCount, registeringCount)
	}
}

func enrollTestWorker(t *testing.T, ctx context.Context, q *db.Queries, workerID uuid.UUID, resourceID string, secretHash []byte) db.EnrollWorkerHostRow {
	t.Helper()
	row, err := q.EnrollWorkerHost(ctx, enrollmentParams(workerID, resourceID, secretHash))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func enrollmentParams(workerID uuid.UUID, resourceID string, secretHash []byte) db.EnrollWorkerHostParams {
	return db.EnrollWorkerHostParams{
		TokenHash:    make([]byte, 32),
		WorkerPoolID: pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)), PoolName: "default",
		WorkerHostID: pgvalue.UUID(workerID), ResourceID: resourceID,
		CurrentServiceID: pgvalue.NewUUIDv7(), HostSecretID: pgvalue.NewUUIDv7(),
		KeyPrefix: uuid.New().String(), SecretHash: secretHash,
	}
}
