package db_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStaleWorkerFenceUsesStateAppropriateStrictBoundaries(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	observationCutoff := now.Add(-time.Duration(workergroup.ObservationFreshnessSeconds) * time.Second)
	registrationCutoff := now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)
	exactID := insertRegisteringWorker(t, ctx, pool, registrationCutoff, false)
	freshUnderActiveCutoffID := insertRegisteringWorker(t, ctx, pool, observationCutoff.Add(-time.Minute), false)
	staleID := insertRegisteringWorker(t, ctx, pool, registrationCutoff.Add(-time.Microsecond), false)
	staleEpochID := insertRegisteringWorker(t, ctx, pool, registrationCutoff.Add(-2*time.Microsecond), true)
	activeExactID := insertActiveWorkerWithObservation(t, ctx, pool, now)
	activeStaleID := insertActiveWorkerWithObservation(t, ctx, pool, now)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txQueries := db.New(tx)
	if _, err := tx.Exec(ctx, `
		UPDATE worker_hosts
		   SET observed_at = CASE id
		       WHEN $1 THEN transaction_timestamp() - $3::bigint * interval '1 second'
		       WHEN $2 THEN transaction_timestamp() - $3::bigint * interval '1 second' - interval '1 microsecond'
		       END
		 WHERE id IN ($1, $2)
	`, activeExactID, activeStaleID, workergroup.ObservationFreshnessSeconds); err != nil {
		t.Fatal(err)
	}
	candidates, err := txQueries.ListStaleWorkerFenceCandidates(ctx, db.ListStaleWorkerFenceCandidatesParams{
		RegistrationStaleBefore:     pgvalue.Timestamptz(registrationCutoff),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		RowLimit:                    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("candidates = %+v, want three workers older than their strict state boundary", candidates)
	}
	byID := make(map[pgtype.UUID]db.ListStaleWorkerFenceCandidatesRow, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	if byID[pgvalue.UUID(staleID)].CurrentEpoch.Valid {
		t.Fatalf("pre-epoch registering candidate epoch = %+v, want NULL", byID[pgvalue.UUID(staleID)].CurrentEpoch)
	}
	if got := byID[pgvalue.UUID(staleEpochID)].CurrentEpoch; !got.Valid || got.Int64 != 1 {
		t.Fatalf("epoch-bearing registering candidate epoch = %+v, want 1", got)
	}
	if got := byID[pgvalue.UUID(activeStaleID)].Status; got != db.WorkerHostStatusActive {
		t.Fatalf("active stale candidate state = %q, want active", got)
	}
	fenced, err := txQueries.RecheckAndFenceStaleWorkerHost(ctx, db.RecheckAndFenceStaleWorkerHostParams{
		ID: pgvalue.UUID(staleID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:               pgtype.Int8{},
		RegistrationStaleBefore:     pgvalue.Timestamptz(registrationCutoff),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		ReasonCode:                  pgtype.Text{String: "worker_observation_stale", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fenced.Status != db.WorkerHostStatusLost {
		t.Fatalf("pre-epoch registering fence state = %q, want lost", fenced.Status)
	}
	fencedWithEpoch, err := txQueries.RecheckAndFenceStaleWorkerHost(ctx, db.RecheckAndFenceStaleWorkerHostParams{
		ID: pgvalue.UUID(staleEpochID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:               pgtype.Int8{Int64: 1, Valid: true},
		RegistrationStaleBefore:     pgvalue.Timestamptz(registrationCutoff),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		ReasonCode:                  pgtype.Text{String: "worker_observation_stale", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fencedWithEpoch.Status != db.WorkerHostStatusLost {
		t.Fatalf("epoch-bearing registering fence state = %q, want lost", fencedWithEpoch.Status)
	}
	fencedActive, err := txQueries.RecheckAndFenceStaleWorkerHost(ctx, db.RecheckAndFenceStaleWorkerHostParams{
		ID: pgvalue.UUID(activeStaleID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:               pgtype.Int8{Int64: 1, Valid: true},
		RegistrationStaleBefore:     pgvalue.Timestamptz(registrationCutoff),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		ReasonCode:                  pgtype.Text{String: "worker_observation_stale", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fencedActive.Status != db.WorkerHostStatusLost {
		t.Fatalf("active stale fence state = %q, want lost", fencedActive.Status)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var exactState db.WorkerHostStatus
	var freshUnderActiveCutoffState db.WorkerHostStatus
	var staleState db.WorkerHostStatus
	var staleEpochState db.WorkerHostStatus
	var activeExactState db.WorkerHostStatus
	var activeStaleState db.WorkerHostStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, exactID).Scan(&exactState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, freshUnderActiveCutoffID).Scan(&freshUnderActiveCutoffState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, staleID).Scan(&staleState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, staleEpochID).Scan(&staleEpochState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, activeExactID).Scan(&activeExactState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, activeStaleID).Scan(&activeStaleState); err != nil {
		t.Fatal(err)
	}
	if exactState != db.WorkerHostStatusRegistering || freshUnderActiveCutoffState != db.WorkerHostStatusRegistering || staleState != db.WorkerHostStatusLost || staleEpochState != db.WorkerHostStatusLost || activeExactState != db.WorkerHostStatusActive || activeStaleState != db.WorkerHostStatusLost {
		t.Fatalf("states exact=%q fresh_under_active_cutoff=%q stale=%q stale_epoch=%q active_exact=%q active_stale=%q", exactState, freshUnderActiveCutoffState, staleState, staleEpochState, activeExactState, activeStaleState)
	}
}

func TestUnobservedActiveWorkerFreshnessStartsAtActivation(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	exactID := insertActiveWorkerWithObservation(t, ctx, pool, now)
	staleID := insertActiveWorkerWithObservation(t, ctx, pool, now)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	if _, err := tx.Exec(ctx, `
		UPDATE worker_hosts
		   SET observed_at = NULL,
		       epoch_started_at = transaction_timestamp() - interval '10 minutes',
		       activated_at = CASE id
		           WHEN $1 THEN transaction_timestamp() - $3::bigint * interval '1 second'
		           WHEN $2 THEN transaction_timestamp() - $3::bigint * interval '1 second' - interval '1 microsecond'
		       END
		 WHERE id IN ($1, $2)
	`, exactID, staleID, workergroup.ObservationFreshnessSeconds); err != nil {
		t.Fatal(err)
	}
	candidates, err := queries.ListStaleWorkerFenceCandidates(ctx, db.ListStaleWorkerFenceCandidatesParams{
		RegistrationStaleBefore:     pgvalue.Timestamptz(now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		RowLimit:                    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != pgvalue.UUID(staleID) {
		t.Fatalf("unobserved active candidates = %+v, want only activation older than the strict boundary", candidates)
	}
	if _, err := queries.RecheckAndFenceStaleWorkerHost(ctx, db.RecheckAndFenceStaleWorkerHostParams{
		ID: pgvalue.UUID(staleID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:               pgtype.Int8{Int64: 1, Valid: true},
		RegistrationStaleBefore:     pgvalue.Timestamptz(now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		ReasonCode:                  pgtype.Text{String: "worker_observation_stale", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var exactState, staleState db.WorkerHostStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, exactID).Scan(&exactState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, staleID).Scan(&staleState); err != nil {
		t.Fatal(err)
	}
	if exactState != db.WorkerHostStatusActive || staleState != db.WorkerHostStatusLost {
		t.Fatalf("states exact=%q stale=%q, want active and lost", exactState, staleState)
	}
}

func TestFreshWorkerObservationWinsAgainstStaleFenceRecheck(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, now.Add(-10*time.Minute))

	observationTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observationTx.Rollback(ctx) }()
	observationQueries := db.New(observationTx)
	if _, err := observationQueries.RecordWorkerObservation(ctx, workerObservation(workerID)); err != nil {
		_ = observationTx.Rollback(ctx)
		t.Fatal(err)
	}

	transactions, err := dispatch.NewPGXStaleWorkerFenceTransactions(pool)
	if err != nil {
		t.Fatal(err)
	}
	fencer, err := dispatch.NewStaleWorkerFencer(
		transactions,
		dispatch.WithStaleWorkerFenceClock(testFenceClock{now: now}),
	)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := fencer.ReconcileOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Selected != 0 || cycle.Fenced != 0 {
		t.Fatalf("cycle while fresh observation owns worker lock = %+v, want candidate skipped", cycle)
	}
	if err := observationTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var state db.WorkerHostStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, workerID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerHostStatusActive {
		t.Fatalf("worker state = %q, want active", state)
	}
}

func TestStaleFenceWinsBeforeLateWorkerObservation(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, now.Add(-10*time.Minute))
	fenceTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fenceTx.Rollback(ctx) }()
	fenceQueries := db.New(fenceTx)
	candidates, err := fenceQueries.ListStaleWorkerFenceCandidates(ctx, db.ListStaleWorkerFenceCandidatesParams{
		RegistrationStaleBefore:     pgvalue.Timestamptz(now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		RowLimit:                    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != pgvalue.UUID(workerID) {
		t.Fatalf("candidates = %+v", candidates)
	}

	observationDone := make(chan error, 1)
	go func() {
		_, observeErr := db.New(pool).RecordWorkerObservation(ctx, workerObservation(workerID))
		observationDone <- observeErr
	}()
	assertBlocked(t, observationDone)
	if _, err := fenceQueries.RecheckAndFenceStaleWorkerHost(ctx, db.RecheckAndFenceStaleWorkerHostParams{
		ID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:               pgtype.Int8{Int64: 1, Valid: true},
		RegistrationStaleBefore:     pgvalue.Timestamptz(now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)),
		ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		ReasonCode:                  pgtype.Text{String: "worker_observation_stale", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fenceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-observationDone; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late observation error = %v, want pgx.ErrNoRows", err)
	}
	var state db.WorkerHostStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM worker_hosts WHERE id = $1`, workerID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerHostStatusLost {
		t.Fatalf("worker state = %q, want lost", state)
	}
}

func TestWorkerObservationFollowsTheLiveEpochThroughDrain(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now())
	if _, err := pool.Exec(ctx, `
		UPDATE worker_hosts
		   SET status = 'draining', draining_at = now()
		 WHERE id = $1
	`, workerID); err != nil {
		t.Fatal(err)
	}
	observed, err := db.New(pool).RecordWorkerObservation(ctx, db.RecordWorkerObservationParams{
		WorkerHostID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		WorkerEpoch:     pgtype.Int8{Int64: 1, Valid: true},
		RunPausedReason: pgtype.Text{String: "startup_recovery_leak", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status != db.WorkerHostStatusDraining || !observed.ObservedAt.Valid || observed.RunPausedReason.String != "startup_recovery_leak" {
		t.Fatalf("draining observation = %+v", observed)
	}
	staleEpoch := workerObservation(workerID)
	staleEpoch.WorkerEpoch = pgtype.Int8{Int64: 2, Valid: true}
	if _, err := db.New(pool).RecordWorkerObservation(ctx, staleEpoch); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale epoch observation error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_hosts
		   SET status = 'termination_ready', termination_ready_at = now()
		 WHERE id = $1
	`, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.New(pool).RecordWorkerObservation(ctx, workerObservation(workerID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("terminal observation error = %v, want pgx.ErrNoRows", err)
	}
}

func insertRegisteringWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, updatedAt time.Time, withEpoch bool) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	var epoch any
	var serviceID any
	var epochStartedAt any
	if withEpoch {
		epoch = int64(1)
		serviceID = uuid.NewV7()
		epochStartedAt = updatedAt
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_hosts (
			id, resource_id, worker_group_id, worker_pool_id, status, updated_at,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, $8, 'registering', $4, $5, $6, $7)
	`, id, "registering-"+id.String(), dbtest.DefaultWorkerGroupID, updatedAt, epoch, serviceID, epochStartedAt,
		dbtest.DefaultWorkerPoolID); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertActiveWorkerWithObservation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, observedAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	serviceID := uuid.NewV7()
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_hosts (
			id, resource_id, worker_group_id, worker_pool_id, status,
			current_epoch, current_service_id, vm_platform_id,
			epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_guest_ephemeral_disk_bytes, max_vm_slots, max_vm_starts,
			cpu_environment, cpu_environment_digest,
			observed_at, epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, $4, 'active',
			1, $5, $6,
			8000, 17179869184, 274877906944,
			4000, 8589934592,
			34359738368, 8, 1,
			'{}'::jsonb, $7,
			$8, $8, $8
		)
	`, id, "active-"+id.String(), dbtest.DefaultWorkerGroupID, dbtest.DefaultWorkerPoolID,
		serviceID, dbtest.DefaultRuntimeID, dbtest.DefaultCPUConfigID, observedAt); err != nil {
		t.Fatal(err)
	}
	return id
}

func workerObservation(workerID uuid.UUID) db.RecordWorkerObservationParams {
	return db.RecordWorkerObservationParams{
		WorkerHostID:  pgvalue.UUID(workerID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		WorkerEpoch:   pgtype.Int8{Int64: 1, Valid: true},
	}
}

func assertBlocked[T any](t *testing.T, done <-chan T) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("operation completed before worker-row lock released: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
}

type testFenceClock struct {
	now time.Time
}

func (clock testFenceClock) Now() time.Time { return clock.now }

func (testFenceClock) Wait(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// Worker status and pauses belong to the caller's lock-time fence; this
// post-lock check only evaluates time.
func TestComputerInstanceWriterLiveChecksWriterAndFreshness(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		live      bool
	}{
		{"active", ``, true},
		{"Run pause is not applied", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak' WHERE id=$1`, true},
		{"status is not applied", `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE id=$1`, true},
		{"stale observation", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, false},
		{"expired writer", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE worker_host_id=$1`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "assigned", time.Now())
			var instanceID pgtype.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&instanceID); err != nil {
				t.Fatal(err)
			}
			if test.sql != "" {
				dbtest.MustExec(t, t.Context(), f.Pool, test.sql, f.WorkerID)
			}
			live, err := db.New(f.Pool).GetComputerInstanceWriterLive(t.Context(), db.GetComputerInstanceWriterLiveParams{
				ID: instanceID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
			})
			if err != nil {
				t.Fatal(err)
			}
			if live != test.live {
				t.Fatalf("writer live = %v, want %v", live, test.live)
			}
		})
	}
}
