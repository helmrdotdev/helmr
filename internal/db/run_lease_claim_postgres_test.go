package db

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var runLeaseTestWorkerGroup = pgvalue.UUID(runtest.WorkerGroupID)

type runLeaseClaimFixture struct {
	pool                 *pgxpool.Pool
	queries              *Queries
	orgID                uuid.UUID
	projectID            uuid.UUID
	environmentID        uuid.UUID
	deploymentID         uuid.UUID
	taskDefinitionID     uuid.UUID
	computerDefinitionID uuid.UUID
	workerID             uuid.UUID
	vmPlatformID         string
	base                 runtest.Fixture
}

type runLeaseWork struct {
	leaseID uuid.UUID
	runID   uuid.UUID
}

func TestRunLeaseClaimReadinessFailsClosedWithoutObservation(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_hosts SET observed_at = NULL WHERE id = $1`,
		fixture.workerID,
	); err != nil {
		t.Fatal(err)
	}

	worker, err := fixture.queries.LockRunLeaseClaimReadyWorker(ctx, LockRunLeaseClaimReadyWorkerParams{
		ID:            pgvalue.UUID(fixture.workerID),
		WorkerGroupID: runLeaseTestWorkerGroup,
		// Mirrors workergroup.ObservationFreshnessSeconds; a literal because this
		// in-package test cannot import workergroup without an import cycle. No
		// freshness window admits a worker without an observation.
		ObservationFreshnessSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worker.RunReady {
		t.Fatal("unobserved worker is ready to claim a run lease")
	}
}

func TestRunLeaseDiscoveryAndClaimFoundation(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	assigned := fixture.addWork(t, ctx, "assigned", time.Now().Add(-2*time.Minute))
	starting := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))

	rows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, RowLimit: 8, WorkerHostID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || pgvalue.MustUUIDValue(rows[0].ID) != starting.leaseID ||
		pgvalue.MustUUIDValue(rows[1].ID) != assigned.leaseID {
		t.Fatalf("discovery = %+v, want starting then assigned", rows)
	}
	var state RunLeaseStatus
	var claimedAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(ctx,
		`SELECT status, claimed_at FROM run_leases WHERE id = $1`, assigned.leaseID,
	).Scan(&state, &claimedAt); err != nil {
		t.Fatal(err)
	}
	if state != RunLeaseStatusAssigned || claimedAt.Valid {
		t.Fatalf("discovery mutated assigned lease to state=%s claimed_at=%v", state, claimedAt)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE computer_instances SET mount_state='failed' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, starting.leaseID); err != nil {
		t.Fatal(err)
	}
	rows, err = fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, RowLimit: 8,
		WorkerHostID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || pgvalue.MustUUIDValue(rows[0].ID) != assigned.leaseID {
		t.Fatalf("discovery after Mount failure = %+v, want only healthy assigned lease", rows)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE computer_instances SET mount_state='mounted' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, starting.leaseID); err != nil {
		t.Fatal(err)
	}

	secretLocators, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(secretLocators.RunID) != assigned.runID ||
		secretLocators.EnvironmentID != pgvalue.UUID(fixture.environmentID) ||
		secretLocators.AttemptNumber != 1 {
		t.Fatalf("Secret delivery locators = %+v", secretLocators)
	}

	locators, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(locators.RunID) != assigned.runID {
		t.Fatalf("locator run = %s, want %s", pgvalue.UUIDString(locators.RunID), assigned.runID)
	}
	if locators.RunWaitID.Valid ||
		locators.SuspendCheckpointID.Valid {
		t.Fatalf("fresh locator exposed restore authority: %+v", locators)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 2,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale sequence locator error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(uuid.NewV7()),
		WorkerEpoch: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-worker locator error = %v, want no rows", err)
	}

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked := New(tx)
	run, err := locked.LockRunLeaseClaimRun(ctx, LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, ComputerID: locators.ComputerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	computer, err := locked.LockRunLeaseClaimComputer(ctx, LockRunLeaseClaimComputerParams{
		ID: locators.ComputerID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimAttempt(ctx, LockRunLeaseClaimAttemptParams{
		RunID: locators.RunID, Number: locators.AttemptNumber, ComputerID: locators.ComputerID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimWorkerGroup(ctx, LockRunLeaseClaimWorkerGroupParams{
		ID: runLeaseTestWorkerGroup, RegionID: locators.RegionID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimWorker(ctx, LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(fixture.workerID), WorkerGroupID: runLeaseTestWorkerGroup,
	}); err != nil {
		t.Fatal(err)
	}
	instance, err := locked.LockRunLeaseClaimInstance(ctx, LockRunLeaseClaimInstanceParams{
		ID: locators.ComputerInstanceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, ComputerID: locators.ComputerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimLease(ctx, LockRunLeaseClaimLeaseParams{
		ID: pgvalue.UUID(assigned.leaseID), RunID: locators.RunID,
		ComputerID: locators.ComputerID, AttemptNumber: locators.AttemptNumber,
		LeaseSequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if run.CurrentRunLeaseID != pgvalue.UUID(assigned.leaseID) || instance.ComputerID != computer.ID || instance.WriterGeneration != computer.WriterGeneration || instance.ID != locators.ComputerInstanceID || instance.WriterGeneration != locators.WriterGeneration || instance.MountState != "mounted" {
		t.Fatal("claim is not attached to the current physical writer")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	claimed, err := fixture.queries.MarkRunLeaseStarting(ctx, MarkRunLeaseStartingParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != RunLeaseStatusStarting || !claimed.ClaimedAt.Valid {
		t.Fatalf("claimed lease = state:%s claimed_at:%v", claimed.Status, claimed.ClaimedAt)
	}
	firstClaimedAt := claimed.ClaimedAt.Time
	if _, err := fixture.queries.MarkRunLeaseStarting(ctx, MarkRunLeaseStartingParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim update error = %v, want no rows", err)
	}
	var replayedClaimedAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(ctx, `
		SELECT claimed_at
		  FROM run_leases
		 WHERE run_id = $1
		   AND attempt_number = 1
		   AND computer_id = $2
		   AND id = $3
	`, pgvalue.UUID(assigned.runID), locators.ComputerID, pgvalue.UUID(assigned.leaseID)).Scan(&replayedClaimedAt); err != nil {
		t.Fatal(err)
	}
	if !replayedClaimedAt.Valid || !replayedClaimedAt.Time.Equal(firstClaimedAt) {
		t.Fatalf("claim replay timestamp = %v, want %s", replayedClaimedAt, firstClaimedAt)
	}
	unclaimed := fixture.addWork(t, ctx, "assigned", time.Now())

	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_hosts SET status = 'draining', draining_at = now() WHERE id = $1`,
		fixture.workerID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_groups SET status = 'draining' WHERE id = $1`,
		runLeaseTestWorkerGroup,
	); err != nil {
		t.Fatal(err)
	}
	drainingRows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, RowLimit: 8, WorkerHostID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(drainingRows) != 3 {
		t.Fatalf("draining discovery returned %d rows, want assigned plus two starting leases", len(drainingRows))
	}
	foundUnclaimed := false
	for _, row := range drainingRows {
		if pgvalue.MustUUIDValue(row.ID) == unclaimed.leaseID {
			foundUnclaimed = true
		}
		if pgvalue.MustUUIDValue(row.ID) != assigned.leaseID &&
			pgvalue.MustUUIDValue(row.ID) != starting.leaseID &&
			pgvalue.MustUUIDValue(row.ID) != unclaimed.leaseID {
			t.Fatalf("draining discovery returned unrelated lease %s", pgvalue.UUIDString(row.ID))
		}
	}
	if !foundUnclaimed {
		t.Fatalf("draining discovery omitted assigned lease %s", unclaimed.leaseID)
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(unclaimed.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining assigned Secret locator: %v", err)
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining replay Secret locator: %v", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining replay claim locator: %v", err)
	}
	if _, err := fixture.queries.GetRunLeaseStartLocators(ctx, GetRunLeaseStartLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining starting lease start locator: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE run_leases
   SET status = 'running', started_at = now(), updated_at = now()
 WHERE id = $1`, pgvalue.UUID(assigned.leaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE runs
   SET status = 'running', started_at = now(), active_started_at = now(), updated_at = now()
 WHERE id = $1`, pgvalue.UUID(assigned.runID)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetLiveRunLeaseLocators(ctx, GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining running lease entrypoint locator: %v", err)
	}
	if _, err := fixture.queries.GetLiveRunLeaseLocators(ctx, GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 2}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale draining entrypoint locator error = %v, want no rows", err)
	}
}

func newRunLeaseClaimFixture(t *testing.T, _ context.Context) runLeaseClaimFixture {
	t.Helper()
	base := runtest.New(t)
	return runLeaseClaimFixture{
		pool:                 base.Pool,
		queries:              New(base.Pool),
		orgID:                base.OrgID,
		projectID:            base.ProjectID,
		environmentID:        base.EnvironmentID,
		deploymentID:         base.DeploymentID,
		taskDefinitionID:     base.TaskDefinitionID,
		computerDefinitionID: base.ComputerDefinitionID,
		workerID:             base.WorkerID,
		vmPlatformID:         base.VMPlatformID,
		base:                 base,
	}
}

func (fixture runLeaseClaimFixture) addWork(
	t *testing.T,
	_ context.Context,
	state string,
	assignedAt time.Time,
) runLeaseWork {
	t.Helper()
	work := fixture.base.AddRunLease(t, state, assignedAt)
	return runLeaseWork{leaseID: work.LeaseID, runID: work.RunID}
}

func (fixture runLeaseClaimFixture) convertToActor(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
	retryPolicy string,
) uuid.UUID {
	t.Helper()
	return fixture.base.ConvertToActor(
		t,
		ctx,
		runtest.RunLease{LeaseID: work.leaseID, RunID: work.runID},
		retryPolicy,
	)
}

func TestRunLeaseDiscoveryOrdersByCreationWithinState(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	old := time.Now().Add(-time.Minute)
	newer := fixture.addWork(t, ctx, "assigned", old.Add(time.Second))
	first := fixture.addWork(t, ctx, "assigned", old)
	second := fixture.addWork(t, ctx, "assigned", old)
	starting := fixture.addWork(t, ctx, "starting", old.Add(2*time.Second))
	if first.leaseID.String() > second.leaseID.String() {
		first, second = second, first
	}
	rows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1, RowLimit: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{starting.leaseID, first.leaseID, second.leaseID, newer.leaseID}
	if len(rows) != len(want) {
		t.Fatalf("discovery count = %d, want %d", len(rows), len(want))
	}
	for i, id := range want {
		if pgvalue.MustUUIDValue(rows[i].ID) != id {
			t.Fatalf("discovery[%d] = %v, want %v", i, rows[i].ID, id)
		}
	}
}
