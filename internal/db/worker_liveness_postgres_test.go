package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStaleWorkerFenceUsesStateAppropriateStrictBoundaries(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	observationCutoff := now.Add(-120 * time.Second)
	registrationCutoff := now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)
	exactID := insertRegisteringWorker(t, ctx, pool, registrationCutoff, false)
	freshUnderActiveCutoffID := insertRegisteringWorker(t, ctx, pool, observationCutoff.Add(-time.Minute), false)
	staleID := insertRegisteringWorker(t, ctx, pool, registrationCutoff.Add(-time.Microsecond), false)
	staleEpochID := insertRegisteringWorker(t, ctx, pool, registrationCutoff.Add(-2*time.Microsecond), true)
	activeExactID := insertActiveWorkerWithObservation(t, ctx, pool, observationCutoff.Add(time.Second))
	activeStaleID := insertActiveWorkerWithObservation(t, ctx, pool, observationCutoff.Add(-time.Microsecond))

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txQueries := db.New(tx)
	candidates, err := txQueries.ListStaleWorkerFenceCandidates(ctx, db.ListStaleWorkerFenceCandidatesParams{
		RegistrationStaleBefore: pgvalue.Timestamptz(registrationCutoff),
		RowLimit:                10,
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
	if got := byID[pgvalue.UUID(activeStaleID)].State; got != db.WorkerInstanceStateActive {
		t.Fatalf("active stale candidate state = %q, want active", got)
	}
	fenced, err := txQueries.RecheckAndFenceStaleWorkerInstance(ctx, db.RecheckAndFenceStaleWorkerInstanceParams{
		ID: pgvalue.UUID(staleID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:           pgtype.Int8{},
		RegistrationStaleBefore: pgvalue.Timestamptz(registrationCutoff),
		ReasonCode:              pgtype.Text{String: "worker_observation_stale", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fenced.State != db.WorkerInstanceStateLost {
		t.Fatalf("pre-epoch registering fence state = %q, want lost", fenced.State)
	}
	fencedWithEpoch, err := txQueries.RecheckAndFenceStaleWorkerInstance(ctx, db.RecheckAndFenceStaleWorkerInstanceParams{
		ID: pgvalue.UUID(staleEpochID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:           pgtype.Int8{Int64: 1, Valid: true},
		RegistrationStaleBefore: pgvalue.Timestamptz(registrationCutoff),
		ReasonCode:              pgtype.Text{String: "worker_observation_stale", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fencedWithEpoch.State != db.WorkerInstanceStateLost {
		t.Fatalf("epoch-bearing registering fence state = %q, want lost", fencedWithEpoch.State)
	}
	fencedActive, err := txQueries.RecheckAndFenceStaleWorkerInstance(ctx, db.RecheckAndFenceStaleWorkerInstanceParams{
		ID: pgvalue.UUID(activeStaleID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:           pgtype.Int8{Int64: 1, Valid: true},
		RegistrationStaleBefore: pgvalue.Timestamptz(registrationCutoff),
		ReasonCode:              pgtype.Text{String: "worker_observation_stale", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fencedActive.State != db.WorkerInstanceStateLost {
		t.Fatalf("active stale fence state = %q, want lost", fencedActive.State)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var exactState db.WorkerInstanceState
	var freshUnderActiveCutoffState db.WorkerInstanceState
	var staleState db.WorkerInstanceState
	var staleEpochState db.WorkerInstanceState
	var activeExactState db.WorkerInstanceState
	var activeStaleState db.WorkerInstanceState
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, exactID).Scan(&exactState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, freshUnderActiveCutoffID).Scan(&freshUnderActiveCutoffState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, staleID).Scan(&staleState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, staleEpochID).Scan(&staleEpochState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, activeExactID).Scan(&activeExactState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, activeStaleID).Scan(&activeStaleState); err != nil {
		t.Fatal(err)
	}
	if exactState != db.WorkerInstanceStateRegistering || freshUnderActiveCutoffState != db.WorkerInstanceStateRegistering || staleState != db.WorkerInstanceStateLost || staleEpochState != db.WorkerInstanceStateLost || activeExactState != db.WorkerInstanceStateActive || activeStaleState != db.WorkerInstanceStateLost {
		t.Fatalf("states exact=%q fresh_under_active_cutoff=%q stale=%q stale_epoch=%q active_exact=%q active_stale=%q", exactState, freshUnderActiveCutoffState, staleState, staleEpochState, activeExactState, activeStaleState)
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
	if _, err := observationQueries.RecordWorkerObservation(ctx, workerObservation(workerID, now)); err != nil {
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
	var state db.WorkerInstanceState
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, workerID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerInstanceStateActive {
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
		RegistrationStaleBefore: pgvalue.Timestamptz(now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)),
		RowLimit:                10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != pgvalue.UUID(workerID) {
		t.Fatalf("candidates = %+v", candidates)
	}

	observationDone := make(chan error, 1)
	go func() {
		_, observeErr := db.New(pool).RecordWorkerObservation(ctx, workerObservation(workerID, now))
		observationDone <- observeErr
	}()
	assertBlocked(t, observationDone)
	if _, err := fenceQueries.RecheckAndFenceStaleWorkerInstance(ctx, db.RecheckAndFenceStaleWorkerInstanceParams{
		ID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:           pgtype.Int8{Int64: 1, Valid: true},
		RegistrationStaleBefore: pgvalue.Timestamptz(now.Add(-dispatch.DefaultWorkerRegistrationReadinessGrace)),
		ReasonCode:              pgtype.Text{String: "worker_observation_stale", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fenceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-observationDone; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late observation error = %v, want pgx.ErrNoRows", err)
	}
	var state db.WorkerInstanceState
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, workerID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerInstanceStateLost {
		t.Fatalf("worker state = %q, want lost", state)
	}
}

func insertRegisteringWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, updatedAt time.Time, withEpoch bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	var epoch any
	var serviceID any
	var epochStartedAt any
	if withEpoch {
		epoch = int64(1)
		serviceID = uuid.Must(uuid.NewV7())
		epochStartedAt = updatedAt
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, state, updated_at,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, 'registering', $4, $5, $6, $7)
	`, id, "registering-"+id.String(), dbtest.DefaultWorkerGroupID, updatedAt, epoch, serviceID, epochStartedAt); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertActiveWorkerWithObservation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, observedAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	runtimeIdentityID := "active-runtime-" + id.String()
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, network_abi
		) VALUES ($1, 'x86_64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'helmr/v0')
	`, runtimeIdentityID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, state,
			current_epoch, current_service_id, supervisor_version, supports_build, runtime_identity_id,
			epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_guest_ephemeral_disk_bytes, max_build_executors,
			epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, 'active',
			1, $4, 'test-worker', true, $6,
			1000, 1073741824, 1073741824,
			1000, 1073741824,
			1073741824, 1,
			$5, $5
		)
	`, id, "active-"+id.String(), dbtest.DefaultWorkerGroupID, serviceID, observedAt, runtimeIdentityID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.New(pool).RecordWorkerObservation(ctx, workerObservation(id, observedAt)); err != nil {
		t.Fatal(err)
	}
	return id
}

func workerObservation(workerID uuid.UUID, observedAt time.Time) db.RecordWorkerObservationParams {
	return db.RecordWorkerObservationParams{
		HealthDetails:    []byte(`{}`),
		ObservedAt:       pgvalue.Timestamptz(observedAt),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		WorkerEpoch:      pgtype.Int8{Int64: 1, Valid: true},
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
