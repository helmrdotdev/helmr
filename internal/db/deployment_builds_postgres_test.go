package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	buildCPU       int64 = 3000
	buildMemory    int64 = 4 << 30
	buildGuestDisk int64 = 32 << 30
)

type deploymentBuildFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	queries       *db.Queries
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	deploymentID  uuid.UUID
	groupID       string
	workerID      uuid.UUID
}

func newDeploymentBuildFixture(t *testing.T) (*deploymentBuildFixture, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)

	var sourceArtifactID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT deployment_source_artifact_id
		  FROM deployments
		 WHERE org_id = $1 AND id = $2
	`, ids.orgID, ids.deploymentID).Scan(&sourceArtifactID); err != nil {
		t.Fatal(err)
	}

	deploymentID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO deployments (
			id, org_id, project_id, environment_id, build_region_id,
			build_node_version, build_runtime_digest, build_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract_version, image_cache_mode,
			version, content_hash, deployment_source_artifact_id, status
		) VALUES (
			$1, $2, $3, $4, $5,
			'24.16.0', decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
			'npm', '11.5.0', decode(repeat('22', 32), 'hex'),
			'helmr.program-build.v0', 'prefer',
			$6, $7, $8, 'queued'
		)
	`, deploymentID, ids.orgID, ids.projectID,
		ids.environmentID, dbtest.DefaultRegionID, "build-"+dbtest.ShortID(deploymentID),
		dbtest.Digest("deployment-build-"+deploymentID.String()), sourceArtifactID)

	groupID := dbtest.DefaultWorkerGroupID
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, state,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, 'registering', 1, $4, now())
	`, workerID, workerID.String(), groupID, serviceID)

	return &deploymentBuildFixture{
		ctx:           ctx,
		pool:          pool,
		queries:       db.New(pool),
		orgID:         ids.orgID,
		projectID:     ids.projectID,
		environmentID: ids.environmentID,
		deploymentID:  deploymentID,
		groupID:       groupID,
		workerID:      workerID,
	}, pool
}

func TestAppendDeploymentEventsPreservesInputOrder(t *testing.T) {
	f, _ := newDeploymentBuildFixture(t)
	inserted, err := f.queries.AppendDeploymentEvents(f.ctx, db.AppendDeploymentEventsParams{
		Categories:       []string{"build", "build", "build"},
		Severities:       []string{"info", "info", "info"},
		Sources:          []string{"worker", "worker", "worker"},
		Kinds:            []string{"deployment.build.exit", "deployment.build.log", "deployment.build.log"},
		Messages:         []string{"exit", "stdout", "stderr"},
		Payloads:         []string{`{"position":0}`, `{"position":1}`, `{"position":2}`},
		RedactionClasses: []string{"sensitive", "sensitive", "sensitive"},
		OrgID:            pgvalue.UUID(f.orgID),
		ProjectID:        pgvalue.UUID(f.projectID),
		EnvironmentID:    pgvalue.UUID(f.environmentID),
		DeploymentID:     pgvalue.UUID(f.deploymentID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 3 {
		t.Fatalf("inserted events = %d, want 3", inserted)
	}
	rows, err := f.pool.Query(f.ctx, `
		SELECT message
		  FROM telemetry_outbox
		 WHERE deployment_id = $1
		   AND kind IN ('deployment.build.exit', 'deployment.build.log')
		 ORDER BY id
	`, f.deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var messages []string
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"exit", "stdout", "stderr"}; !reflect.DeepEqual(messages, want) {
		t.Fatalf("event order = %v, want %v", messages, want)
	}
}

func TestPinDeploymentPlatformArtifactsReplaysExactTuple(t *testing.T) {
	f, _ := newDeploymentBuildFixture(t)
	dbtest.MustExec(t, f.ctx, f.pool, `
		UPDATE deployments
		   SET build_runtime_digest = NULL,
		       build_toolchain_digest = NULL,
		       build_manager_digest = NULL
		 WHERE id = $1
	`, f.deploymentID)
	pins := db.PinDeploymentPlatformArtifactsParams{
		BuildRuntimeDigest:   bytes.Repeat([]byte{1}, 32),
		BuildToolchainDigest: bytes.Repeat([]byte{2}, 32),
		BuildManagerDigest:   bytes.Repeat([]byte{3}, 32),
		OrgID:                pgvalue.UUID(f.orgID),
		ProjectID:            pgvalue.UUID(f.projectID),
		EnvironmentID:        pgvalue.UUID(f.environmentID),
		ID:                   pgvalue.UUID(f.deploymentID),
	}
	first, err := f.queries.PinDeploymentPlatformArtifacts(f.ctx, pins)
	if err != nil {
		t.Fatal(err)
	}
	var firstTupleID, firstTransactionID string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT ctid::text, xmin::text
		  FROM deployments
		 WHERE id = $1
	`, f.deploymentID).Scan(&firstTupleID, &firstTransactionID); err != nil {
		t.Fatal(err)
	}
	replayed, err := f.queries.PinDeploymentPlatformArtifacts(f.ctx, pins)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.UpdatedAt.Time.Equal(first.UpdatedAt.Time) {
		t.Fatalf("replay changed updated_at from %s to %s", first.UpdatedAt.Time, replayed.UpdatedAt.Time)
	}
	var replayedTupleID, replayedTransactionID string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT ctid::text, xmin::text
		  FROM deployments
		 WHERE id = $1
	`, f.deploymentID).Scan(&replayedTupleID, &replayedTransactionID); err != nil {
		t.Fatal(err)
	}
	if replayedTupleID != firstTupleID || replayedTransactionID != firstTransactionID {
		t.Fatalf(
			"replay rewrote tuple from %s/%s to %s/%s",
			firstTupleID,
			firstTransactionID,
			replayedTupleID,
			replayedTransactionID,
		)
	}
	pins.BuildManagerDigest = bytes.Repeat([]byte{4}, 32)
	if _, err := f.queries.PinDeploymentPlatformArtifacts(f.ctx, pins); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("different pin tuple error = %v", err)
	}
}

func TestPlatformAcquisitionRequiresActiveBuildAuthority(t *testing.T) {
	f, _ := newDeploymentBuildFixture(t)
	dbtest.MustExec(t, f.ctx, f.pool, `
		UPDATE deployments
		   SET build_runtime_digest = NULL,
		       build_toolchain_digest = NULL,
		       build_manager_digest = NULL
		 WHERE id = $1
	`, f.deploymentID)
	f.activateBuildWorker(t)
	params := db.GetDeploymentPlatformAcquisitionParams{
		WorkerInstanceID:      pgvalue.UUID(f.workerID),
		WorkerGroupID:         f.groupID,
		WorkerEpoch:           pgtype.Int8{Int64: 1, Valid: true},
		WorkerProtocolVersion: "helmr.worker.v0",
		ID:                    pgvalue.UUID(f.deploymentID),
	}
	if _, err := f.queries.GetDeploymentPlatformAcquisition(f.ctx, params); err != nil {
		t.Fatal(err)
	}

	dbtest.MustExec(t, f.ctx, f.pool, `
		UPDATE worker_groups
		   SET state = 'disabled'
		 WHERE id = $1
	`, f.groupID)
	if _, err := f.queries.GetDeploymentPlatformAcquisition(
		f.ctx,
		params,
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled worker group error = %v", err)
	}

}

func TestDeploymentLeaseRejectsUntilPinCommits(t *testing.T) {
	f, _ := newDeploymentBuildFixture(t)
	dbtest.MustExec(t, f.ctx, f.pool, `
		UPDATE deployments
		   SET build_runtime_digest = NULL,
		       build_toolchain_digest = NULL,
		       build_manager_digest = NULL
		 WHERE id = $1
	`, f.deploymentID)
	pinTx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pinTx.Rollback(f.ctx)
	pins := db.PinDeploymentPlatformArtifactsParams{
		BuildRuntimeDigest:   bytes.Repeat([]byte{1}, 32),
		BuildToolchainDigest: bytes.Repeat([]byte{2}, 32),
		BuildManagerDigest:   bytes.Repeat([]byte{3}, 32),
		OrgID:                pgvalue.UUID(f.orgID),
		ProjectID:            pgvalue.UUID(f.projectID),
		EnvironmentID:        pgvalue.UUID(f.environmentID),
		ID:                   pgvalue.UUID(f.deploymentID),
	}
	if _, err := db.New(pinTx).PinDeploymentPlatformArtifacts(f.ctx, pins); err != nil {
		t.Fatal(err)
	}

	type leaseResult struct {
		row db.LeaseQueuedDeploymentBuildRow
		err error
	}
	result := make(chan leaseResult, 1)
	go func() {
		row, err := f.queries.LeaseQueuedDeploymentBuild(f.ctx, f.leaseParams(1))
		result <- leaseResult{row: row, err: err}
	}()
	select {
	case early := <-result:
		if !errors.Is(early.err, pgx.ErrNoRows) {
			t.Fatalf("lease before pin commit = %+v, %v", early.row, early.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent lease did not resolve against the unpinned row")
	}
	var visiblePins int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*)
		  FROM deployments
		 WHERE id = $1
		   AND build_runtime_digest IS NOT NULL
		   AND build_toolchain_digest IS NOT NULL
		   AND build_manager_digest IS NOT NULL
	`, f.deploymentID).Scan(&visiblePins); err != nil {
		t.Fatal(err)
	}
	if visiblePins != 0 {
		t.Fatal("uncommitted Platform pins became visible")
	}
	if err := pinTx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	leased, err := f.queries.LeaseQueuedDeploymentBuild(f.ctx, f.leaseParams(1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leased.BuildRuntimeDigest, pins.BuildRuntimeDigest) ||
		!bytes.Equal(
			leased.BuildToolchainDigest,
			pins.BuildToolchainDigest,
		) {
		t.Fatalf("lease used incomplete Platform pins: %+v", leased)
	}
}

func (f *deploymentBuildFixture) lease(t *testing.T, sequence int64) db.LeaseQueuedDeploymentBuildRow {
	t.Helper()
	row, err := f.queries.LeaseQueuedDeploymentBuild(f.ctx, f.leaseParams(sequence))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *deploymentBuildFixture) leaseParams(sequence int64) db.LeaseQueuedDeploymentBuildParams {
	now := time.Now().UTC()
	return db.LeaseQueuedDeploymentBuildParams{
		OrgID:                            pgvalue.UUID(f.orgID),
		DeploymentID:                     pgvalue.UUID(f.deploymentID),
		BuildRegionID:                    dbtest.DefaultRegionID,
		RequestedCPUMillis:               buildCPU,
		RequestedMemoryBytes:             buildMemory,
		RequestedGuestEphemeralDiskBytes: buildGuestDisk,
		RequestedBuildExecutors:          1,
		BuildLeaseID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		LeaseSequence:                    sequence,
		WorkerGroupID:                    f.groupID,
		BuildWorkerInstanceID:            pgvalue.UUID(f.workerID),
		WorkerEpoch:                      1,
		WorkerProtocolVersion:            "helmr.worker.v0",
		BuildSnapshot:                    []byte(`{"source":"test"}`),
		StartDeadlineAt:                  pgvalue.Timestamptz(now.Add(time.Minute)),
		BuildLeaseExpiresAt:              pgvalue.Timestamptz(now.Add(5 * time.Minute)),
	}
}

func (f *deploymentBuildFixture) activateBuildWorker(t *testing.T) {
	t.Helper()
	runtimeIdentityID := "build-runtime-" + dbtest.ShortID(f.workerID)
	dbtest.MustExec(t, f.ctx, f.pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, network_abi
		) VALUES (
			$1, 'x86_64', 'helmr.runtime.v0', 'sha256:test-kernel',
			'sha256:test-initramfs', 'sha256:test-rootfs', 'helmr/v0'
		)
	`, runtimeIdentityID)
	dbtest.MustExec(t, f.ctx, f.pool, `
		UPDATE worker_instances
		   SET state = 'active',
		       supports_build = true,
		       runtime_identity_id = $2,
		       supervisor_version = 'test-worker',
		       epoch_cpu_millis = $3,
		       epoch_memory_bytes = $4,
		       epoch_guest_ephemeral_disk_bytes = $5,
		       per_vm_cpu_millis = $3,
		       per_vm_memory_bytes = $4,
		       per_vm_guest_ephemeral_disk_bytes = $5,
		       max_build_executors = 1,
		       activated_at = now()
		 WHERE id = $1
	`, f.workerID, runtimeIdentityID, buildCPU, buildMemory, buildGuestDisk)
	dbtest.MustExec(t, f.ctx, f.pool, `
		INSERT INTO worker_observations (
			worker_instance_id, worker_epoch,
			cpu_pressure_bps, memory_pressure_bps,
			guest_ephemeral_disk_pressure_bps,
			build_cache_pressure_bps, artifact_cache_pressure_bps,
			checkpoint_pressure_bps, quarantined_resource_count, run_queue_depth,
			build_queue_depth, runtime_start_queue_depth, observed_at
		) VALUES (
			$1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, now()
		)
	`, f.workerID)
}

func TestClaimNextDeploymentBuildLeaseRechecksGroupAdmission(t *testing.T) {
	for _, test := range []struct {
		name       string
		groupState string
		wantClaim  bool
	}{
		{name: "active", groupState: "active", wantClaim: true},
		{name: "draining", groupState: "draining", wantClaim: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _ := newDeploymentBuildFixture(t)
			f.activateBuildWorker(t)
			lease := f.lease(t, 1)
			leaseID := pgvalue.MustUUIDValue(lease.ID)
			dbtest.MustExec(t, f.ctx, f.pool, `
				UPDATE worker_groups
				   SET state = $2
				 WHERE id = $1
			`, f.groupID, test.groupState)

			claimed, err := f.queries.ClaimNextDeploymentBuildLease(
				f.ctx,
				db.ClaimNextDeploymentBuildLeaseParams{
					WorkerGroupID:         f.groupID,
					WorkerInstanceID:      pgvalue.UUID(f.workerID),
					WorkerEpoch:           1,
					WorkerProtocolVersion: "helmr.worker.v0",
					ExpiresAt: pgvalue.Timestamptz(
						time.Now().UTC().Add(10 * time.Minute),
					),
				},
			)
			if test.wantClaim {
				if err != nil {
					t.Fatal(err)
				}
				if pgvalue.MustUUIDValue(claimed.ID) != leaseID ||
					claimed.State != db.DeploymentBuildLeaseStateStarting {
					t.Fatalf("claimed lease = %+v", claimed)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim error = %v, want pgx.ErrNoRows", err)
			}
			var state string
			if err := f.pool.QueryRow(f.ctx, `
				SELECT state
				  FROM deployment_build_leases
				 WHERE id = $1
			`, leaseID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != db.DeploymentBuildLeaseStateAssigned {
				t.Fatalf("lease state = %q, want assigned", state)
			}
		})
	}
}

func (f *deploymentBuildFixture) start(t *testing.T, leaseID uuid.UUID, sequence int64) db.DeploymentBuildLease {
	t.Helper()
	dbtest.MustExec(t, f.ctx, f.pool, `
		UPDATE deployment_build_leases
		   SET state = 'starting', claimed_at = now(), renewed_at = now()
		 WHERE id = $1
	`, leaseID)
	row, err := f.queries.StartDeploymentBuildLease(f.ctx, db.StartDeploymentBuildLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(time.Now().UTC().Add(10 * time.Minute)),
		OrgID:     pgvalue.UUID(f.orgID), DeploymentID: pgvalue.UUID(f.deploymentID),
		BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: sequence,
		WorkerGroupID: f.groupID, WorkerInstanceID: pgvalue.UUID(f.workerID),
		WorkerEpoch: 1, RequestedGuestEphemeralDiskBytes: buildGuestDisk,
		RequestedCPUMillis:   buildCPU,
		RequestedMemoryBytes: buildMemory, RequestedBuildExecutors: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *deploymentBuildFixture) reject(t *testing.T, leaseID uuid.UUID, sequence int64) {
	t.Helper()
	_, err := f.queries.RejectDeploymentBuildLease(f.ctx, db.RejectDeploymentBuildLeaseParams{
		OrgID: pgvalue.UUID(f.orgID), DeploymentID: pgvalue.UUID(f.deploymentID),
		BuildLeaseID:               pgvalue.UUID(leaseID),
		ReasonCode:                 pgtype.Text{String: "worker_preflight_rejected", Valid: true},
		TerminalRequestFingerprint: "sha256:7f597b648818c6c44c38b69b6198f7efee4c68f922d3a13398d64f9ff330c891",
		LeaseSequence:              sequence, WorkerGroupID: f.groupID,
		WorkerInstanceID: pgvalue.UUID(f.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *deploymentBuildFixture) failDelivery(t *testing.T, leaseID uuid.UUID, sequence int64) db.FailDeploymentBuildDeliveryRow {
	t.Helper()
	row, err := f.queries.FailDeploymentBuildDelivery(f.ctx, db.FailDeploymentBuildDeliveryParams{
		OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID),
		EnvironmentID: pgvalue.UUID(f.environmentID),
		DeploymentID:  pgvalue.UUID(f.deploymentID), BuildLeaseID: pgvalue.UUID(leaseID),
		LeaseSequence: sequence, WorkerGroupID: f.groupID,
		WorkerInstanceID: pgvalue.UUID(f.workerID), WorkerEpoch: 1,
		WorkerProtocolVersion: "helmr.worker.v0",
		ReasonCode: pgtype.Text{
			String: "program_verifier_failed",
			Valid:  true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *deploymentBuildFixture) complete(t *testing.T, leaseID uuid.UUID, sequence int64) db.CompleteDeploymentBuildRow {
	t.Helper()
	row, err := f.queries.CompleteDeploymentBuild(f.ctx, db.CompleteDeploymentBuildParams{
		TerminalRequestFingerprint: "sha256:complete-" + leaseID.String(),
		OrgID:                      pgvalue.UUID(f.orgID),
		ID:                         pgvalue.UUID(f.deploymentID),
		BuildLeaseID:               pgvalue.UUID(leaseID),
		BuildWorkerInstanceID:      pgvalue.UUID(f.workerID),
		WorkerEpoch:                1,
		LeaseSequence:              sequence,
		QueueConfig:                []byte(`{"formatVersion":0,"queues":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestDeploymentBuildDeliveryRedrivesAndExhausts(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)

	first := f.lease(t, 1)
	firstID := pgvalue.MustUUIDValue(first.ID)
	if first.LeaseSequence != 1 || first.DeploymentStatus != db.DeploymentStatusBuilding {
		t.Fatalf("first delivery = sequence %d status %s", first.LeaseSequence, first.DeploymentStatus)
	}
	f.start(t, firstID, 1)
	lost := f.failDelivery(t, firstID, 1)
	if lost.Replayed || lost.State != db.DeploymentBuildLeaseStateLost ||
		lost.DeploymentStatus != db.DeploymentStatusBuilding {
		t.Fatalf("first delivery failure = %+v", lost)
	}
	if !lost.TerminalReasonCode.Valid || lost.TerminalReasonCode.String != "program_verifier_failed" {
		t.Fatalf("first delivery reason = %v", lost.TerminalReasonCode)
	}

	second := f.lease(t, 2)
	secondID := pgvalue.MustUUIDValue(second.ID)
	replay := f.failDelivery(t, firstID, 1)
	if !replay.Replayed || replay.State != db.DeploymentBuildLeaseStateLost {
		t.Fatalf("delivery-failure replay = %+v", replay)
	}
	var current pgtype.UUID
	if err := pool.QueryRow(f.ctx, `
		SELECT current_build_lease_id FROM deployments WHERE id = $1
	`, f.deploymentID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(current) != secondID {
		t.Fatalf("replay replaced current delivery: %s", pgvalue.UUIDString(current))
	}

	f.reject(t, secondID, 2)
	third := f.lease(t, 3)
	thirdID := pgvalue.MustUUIDValue(third.ID)
	f.start(t, thirdID, 3)
	exhausted := f.failDelivery(t, thirdID, 3)
	if exhausted.Replayed || exhausted.DeploymentStatus != db.DeploymentStatusFailed {
		t.Fatalf("final delivery failure = %+v", exhausted)
	}

	var status db.DeploymentStatus
	var failure []byte
	if err := pool.QueryRow(f.ctx, `
		SELECT status, failure, current_build_lease_id
		  FROM deployments
		 WHERE id = $1
	`, f.deploymentID).Scan(&status, &failure, &current); err != nil {
		t.Fatal(err)
	}
	if status != db.DeploymentStatusFailed || pgvalue.MustUUIDValue(current) != thirdID {
		t.Fatalf("exhausted deployment = status %s pointer %s", status, pgvalue.UUIDString(current))
	}
	var failureBody map[string]string
	if err := json.Unmarshal(failure, &failureBody); err != nil {
		t.Fatal(err)
	}
	if failureBody["reason_code"] != "build_delivery_exhausted" {
		t.Fatalf("exhaustion failure = %s", failure)
	}

	var meterCount int
	var nonnullAttempts int
	if err := pool.QueryRow(f.ctx, `
		SELECT count(*), count(attempt_number)
		  FROM meter_events
		 WHERE deployment_id = $1
	`, f.deploymentID).Scan(&meterCount, &nonnullAttempts); err != nil {
		t.Fatal(err)
	}
	if meterCount != 2 || nonnullAttempts != 0 {
		t.Fatalf("delivery meters = %d rows, %d with attempts", meterCount, nonnullAttempts)
	}

	firstReplay := f.failDelivery(t, firstID, 1)
	if !firstReplay.Replayed || firstReplay.DeploymentStatus != db.DeploymentStatusFailed {
		t.Fatalf("first delivery replay after exhaustion = %+v", firstReplay)
	}
	if err := pool.QueryRow(f.ctx, `
		SELECT status, current_build_lease_id
		  FROM deployments
		 WHERE id = $1
	`, f.deploymentID).Scan(&status, &current); err != nil {
		t.Fatal(err)
	}
	if status != db.DeploymentStatusFailed || pgvalue.MustUUIDValue(current) != thirdID {
		t.Fatalf("old replay changed exhausted deployment = status %s pointer %s", status, pgvalue.UUIDString(current))
	}
}

func TestLostDeploymentBuildDeliveryReplayPreservesReplacementSuccess(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)

	first := f.lease(t, 1)
	firstID := pgvalue.MustUUIDValue(first.ID)
	f.start(t, firstID, 1)
	f.failDelivery(t, firstID, 1)

	second := f.lease(t, 2)
	secondID := pgvalue.MustUUIDValue(second.ID)
	f.start(t, secondID, 2)
	completed := f.complete(t, secondID, 2)
	if completed.Status != db.DeploymentStatusDeployed {
		t.Fatalf("replacement completion = %+v", completed)
	}

	replay := f.failDelivery(t, firstID, 1)
	if !replay.Replayed || replay.State != db.DeploymentBuildLeaseStateLost ||
		replay.DeploymentStatus != db.DeploymentStatusDeployed {
		t.Fatalf("old delivery replay after replacement success = %+v", replay)
	}

	var status db.DeploymentStatus
	var current pgtype.UUID
	if err := pool.QueryRow(f.ctx, `
		SELECT status, current_build_lease_id
		  FROM deployments
		 WHERE id = $1
	`, f.deploymentID).Scan(&status, &current); err != nil {
		t.Fatal(err)
	}
	if status != db.DeploymentStatusDeployed || pgvalue.MustUUIDValue(current) != secondID {
		t.Fatalf("old replay changed deployed replacement = status %s pointer %s", status, pgvalue.UUIDString(current))
	}
}

func TestDeploymentBuildCompletionPublishesSingleProgramAuthority(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)
	leased := f.lease(t, 1)
	leaseID := pgvalue.MustUUIDValue(leased.ID)
	f.start(t, leaseID, 1)

	artifactID := uuid.Must(uuid.NewV7())
	digest := dbtest.Digest("deployment-program-" + artifactID.String())
	const mediaType = "application/vnd.helmr.deployment-program.v0+squashfs"
	dbtest.MustExec(t, f.ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 123, $3)
	`, f.orgID, digest, mediaType)
	dbtest.MustExec(t, f.ctx, pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind,
			size_bytes, media_type, created_by_worker_instance_id
		) VALUES ($1, $2, $3, $4, $5, 'deployment_program', 123, $6, $7)
	`, artifactID, f.orgID, f.projectID, f.environmentID, digest, mediaType, f.workerID)

	programIndexDigest := make([]byte, 32)
	for position := range programIndexDigest {
		programIndexDigest[position] = 3
	}
	completed, err := f.queries.CompleteDeploymentBuild(
		f.ctx,
		db.CompleteDeploymentBuildParams{
			TerminalRequestFingerprint: "sha256:complete-" + leaseID.String(),
			OrgID:                      pgvalue.UUID(f.orgID),
			ID:                         pgvalue.UUID(f.deploymentID),
			BuildLeaseID:               pgvalue.UUID(leaseID),
			BuildWorkerInstanceID:      pgvalue.UUID(f.workerID),
			WorkerEpoch:                1,
			LeaseSequence:              1,
			ProgramArtifactID:          pgvalue.UUID(artifactID),
			ProgramIndexDigest:         programIndexDigest,
			QueueConfig:                []byte(`{"formatVersion":0,"queues":[]}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != db.DeploymentStatusDeployed ||
		pgvalue.MustUUIDValue(completed.ProgramArtifactID) != artifactID ||
		completed.ProgramArtifactKind != db.ArtifactKindDeploymentProgram ||
		!bytes.Equal(completed.ProgramIndexDigest, programIndexDigest) {
		t.Fatalf("completed Program authority = %+v", completed)
	}

	var programArtifactCount int
	if err := pool.QueryRow(f.ctx, `
		SELECT count(*)
		  FROM artifacts
		 WHERE environment_id = $1
		   AND id = $2
		   AND kind = 'deployment_program'
	`, f.environmentID, artifactID).Scan(&programArtifactCount); err != nil {
		t.Fatal(err)
	}
	if programArtifactCount != 1 {
		t.Fatalf("Program Artifact rows = %d, want 1", programArtifactCount)
	}
}

func TestExpiredDeploymentBuildDeliveryUsesTheSameBoundedPolicy(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)

	first := f.lease(t, 1)
	firstID := pgvalue.MustUUIDValue(first.ID)
	f.start(t, firstID, 1)
	dbtest.MustExec(t, f.ctx, pool, `
		UPDATE deployment_build_leases
		   SET assigned_at = now() - interval '3 minutes',
		       start_deadline_at = now() - interval '2 minutes',
		       claimed_at = now() - interval '110 seconds',
		       started_at = now() - interval '90 seconds',
		       renewed_at = now() - interval '75 seconds',
		       expires_at = now() - interval '1 minute'
		 WHERE id = $1
	`, firstID)
	if err := f.queries.RequeueExpiredDeploymentBuildLeases(f.ctx); err != nil {
		t.Fatal(err)
	}

	var state db.DeploymentBuildLeaseState
	var status db.DeploymentStatus
	var current pgtype.UUID
	var startedAt pgtype.Timestamptz
	if err := pool.QueryRow(f.ctx, `
		SELECT lease.state, lease.started_at, deployment.status, deployment.current_build_lease_id
		  FROM deployment_build_leases lease
		  JOIN deployments deployment ON deployment.id = lease.deployment_id
		 WHERE lease.id = $1
	`, firstID).Scan(&state, &startedAt, &status, &current); err != nil {
		t.Fatal(err)
	}
	if state != db.DeploymentBuildLeaseStateExpired || !startedAt.Valid ||
		status != db.DeploymentStatusBuilding || current.Valid {
		t.Fatalf("first expiry = lease %s started %v deployment %s pointer %s", state, startedAt.Valid, status, pgvalue.UUIDString(current))
	}
	var firstMeterCount int
	if err := pool.QueryRow(f.ctx, `
		SELECT count(*) FROM meter_events WHERE deployment_build_lease_id = $1
	`, firstID).Scan(&firstMeterCount); err != nil {
		t.Fatal(err)
	}
	if firstMeterCount != 1 {
		t.Fatalf("running expiry meters = %d", firstMeterCount)
	}

	second := f.lease(t, 2)
	f.reject(t, pgvalue.MustUUIDValue(second.ID), 2)
	third := f.lease(t, 3)
	thirdID := pgvalue.MustUUIDValue(third.ID)
	dbtest.MustExec(t, f.ctx, pool, `
		UPDATE deployment_build_leases
		   SET assigned_at = now() - interval '3 minutes',
		       start_deadline_at = now() - interval '2 minutes',
		       expires_at = now() - interval '1 minute'
		 WHERE id = $1
	`, thirdID)
	if err := f.queries.RequeueExpiredDeploymentBuildLeases(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(f.ctx, `
		SELECT lease.state, deployment.status, deployment.current_build_lease_id
		  FROM deployment_build_leases lease
		  JOIN deployments deployment ON deployment.id = lease.deployment_id
		 WHERE lease.id = $1
	`, thirdID).Scan(&state, &status, &current); err != nil {
		t.Fatal(err)
	}
	if state != db.DeploymentBuildLeaseStateExpired || status != db.DeploymentStatusFailed ||
		pgvalue.MustUUIDValue(current) != thirdID {
		t.Fatalf("final expiry = lease %s deployment %s pointer %s", state, status, pgvalue.UUIDString(current))
	}

	if _, err := pool.Exec(f.ctx, `
		UPDATE deployment_build_leases SET lease_sequence = 4 WHERE id = $1
	`, thirdID); err == nil {
		t.Fatal("lease sequence 4 must be rejected")
	}
}

func TestDeploymentBuildLogicalFailureIsTerminal(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)
	leased := f.lease(t, 1)
	leaseID := pgvalue.MustUUIDValue(leased.ID)
	started := f.start(t, leaseID, 1)

	recovered, err := f.queries.GetStartedDeploymentBuildLease(f.ctx, db.GetStartedDeploymentBuildLeaseParams{
		OrgID: pgvalue.UUID(f.orgID), DeploymentID: pgvalue.UUID(f.deploymentID),
		BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: 1,
		WorkerGroupID: f.groupID, WorkerInstanceID: pgvalue.UUID(f.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: "helmr.worker.v0",
		RequestedGuestEphemeralDiskBytes: buildGuestDisk,
		RequestedCPUMillis:               buildCPU, RequestedMemoryBytes: buildMemory,
		RequestedBuildExecutors: 1,
	})
	if err != nil || recovered.State != db.DeploymentBuildLeaseStateRunning ||
		recovered.ExpiresAt != started.ExpiresAt {
		t.Fatalf("start response-loss recovery = %+v err=%v", recovered, err)
	}

	fingerprint := "sha256:cf597b648818c6c44c38b69b6198f7efee4c68f922d3a13398d64f9ff330c891"
	failure := []byte(`{"message":"deterministic failure"}`)
	failParams := db.FailDeploymentBuildParams{
		Failure:                    failure,
		ReasonCode:                 pgtype.Text{String: "worker_reported_failure", Valid: true},
		TerminalRequestFingerprint: fingerprint,
		OrgID:                      pgvalue.UUID(f.orgID), ID: pgvalue.UUID(f.deploymentID),
		BuildLeaseID:          pgvalue.UUID(leaseID),
		BuildWorkerInstanceID: pgvalue.UUID(f.workerID),
		WorkerEpoch:           1, LeaseSequence: 1,
	}
	if _, err := f.queries.FailDeploymentBuild(f.ctx, failParams); err != nil {
		t.Fatal(err)
	}
	terminal, err := f.queries.GetDeploymentBuildTerminalResult(f.ctx, db.GetDeploymentBuildTerminalResultParams{
		OrgID: pgvalue.UUID(f.orgID), DeploymentID: pgvalue.UUID(f.deploymentID),
		BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: 1,
		WorkerGroupID: f.groupID, WorkerInstanceID: pgvalue.UUID(f.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: "helmr.worker.v0",
	})
	if err != nil || terminal.State != db.DeploymentBuildLeaseStateFailed ||
		!terminal.TerminalRequestFingerprint.Valid ||
		terminal.TerminalRequestFingerprint.String != fingerprint {
		t.Fatalf("terminal response-loss recovery = %+v err=%v", terminal, err)
	}
	if _, err := f.queries.FailDeploymentBuild(f.ctx, failParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate terminal mutation error = %v, want pgx.ErrNoRows", err)
	}

	var attempt pgtype.Int4
	if err := pool.QueryRow(f.ctx, `
		SELECT attempt_number
		  FROM meter_events
		 WHERE deployment_build_lease_id = $1
	`, leaseID).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if attempt.Valid {
		t.Fatalf("build meter has run attempt number %d", attempt.Int32)
	}
}

func TestDeploymentBuildCannotFailBeforeRunning(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)
	leased := f.lease(t, 1)
	leaseID := pgvalue.MustUUIDValue(leased.ID)
	dbtest.MustExec(t, f.ctx, pool, `
		UPDATE deployment_build_leases
		   SET state = 'starting', claimed_at = now(), renewed_at = now()
		 WHERE id = $1
	`, leaseID)
	_, err := f.queries.FailDeploymentBuild(f.ctx, db.FailDeploymentBuildParams{
		Failure:                    []byte(`{"message":"not started"}`),
		ReasonCode:                 pgtype.Text{String: "worker_reported_failure", Valid: true},
		TerminalRequestFingerprint: "sha256:df597b648818c6c44c38b69b6198f7efee4c68f922d3a13398d64f9ff330c891",
		OrgID:                      pgvalue.UUID(f.orgID),
		ID:                         pgvalue.UUID(f.deploymentID),
		BuildLeaseID:               pgvalue.UUID(leaseID),
		BuildWorkerInstanceID:      pgvalue.UUID(f.workerID),
		WorkerEpoch:                1,
		LeaseSequence:              1,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("starting build failure = %v, want pgx.ErrNoRows", err)
	}
	var state db.DeploymentBuildLeaseState
	var status db.DeploymentStatus
	if err := pool.QueryRow(f.ctx, `
		SELECT lease.state, deployment.status
		  FROM deployment_build_leases lease
		  JOIN deployments deployment ON deployment.id = lease.deployment_id
		 WHERE lease.id = $1
	`, leaseID).Scan(&state, &status); err != nil {
		t.Fatal(err)
	}
	if state != db.DeploymentBuildLeaseStateStarting || status != db.DeploymentStatusBuilding {
		t.Fatalf("starting failure changed state = lease %s deployment %s", state, status)
	}
}

func TestDeploymentBuildTerminalFenceSerializesDeliveryFailure(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)
	leased := f.lease(t, 1)
	leaseID := pgvalue.MustUUIDValue(leased.ID)
	f.start(t, leaseID, 1)

	tx, err := pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	txQueries := db.New(tx)
	_, err = txQueries.LockDeploymentBuildTerminalFence(f.ctx, db.LockDeploymentBuildTerminalFenceParams{
		OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID),
		EnvironmentID: pgvalue.UUID(f.environmentID), DeploymentID: pgvalue.UUID(f.deploymentID),
		BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: 1,
		WorkerGroupID: f.groupID, WorkerInstanceID: pgvalue.UUID(f.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: "helmr.worker.v0",
	})
	if err != nil {
		t.Fatal(err)
	}

	deliveryStarted := make(chan struct{})
	deliveryDone := make(chan error, 1)
	go func() {
		close(deliveryStarted)
		_, err := f.queries.FailDeploymentBuildDelivery(f.ctx, db.FailDeploymentBuildDeliveryParams{
			OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID),
			EnvironmentID: pgvalue.UUID(f.environmentID), DeploymentID: pgvalue.UUID(f.deploymentID),
			BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: 1,
			WorkerGroupID: f.groupID, WorkerInstanceID: pgvalue.UUID(f.workerID),
			WorkerEpoch: 1, WorkerProtocolVersion: "helmr.worker.v0",
		})
		deliveryDone <- err
	}()
	<-deliveryStarted
	time.Sleep(50 * time.Millisecond)

	_, err = txQueries.FailDeploymentBuild(f.ctx, db.FailDeploymentBuildParams{
		Failure:                    []byte(`{"message":"deterministic failure"}`),
		ReasonCode:                 pgtype.Text{String: "worker_reported_failure", Valid: true},
		TerminalRequestFingerprint: "sha256:ef597b648818c6c44c38b69b6198f7efee4c68f922d3a13398d64f9ff330c891",
		OrgID:                      pgvalue.UUID(f.orgID),
		ID:                         pgvalue.UUID(f.deploymentID),
		BuildLeaseID:               pgvalue.UUID(leaseID),
		BuildWorkerInstanceID:      pgvalue.UUID(f.workerID),
		WorkerEpoch:                1,
		LeaseSequence:              1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deliveryDone; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("delivery failure after logical completion = %v, want pgx.ErrNoRows", err)
	}

	var leaseState db.DeploymentBuildLeaseState
	var deploymentStatus db.DeploymentStatus
	if err := pool.QueryRow(f.ctx, `
		SELECT lease.state, deployment.status
		  FROM deployment_build_leases lease
		  JOIN deployments deployment ON deployment.id = lease.deployment_id
		 WHERE lease.id = $1
	`, leaseID).Scan(&leaseState, &deploymentStatus); err != nil {
		t.Fatal(err)
	}
	if leaseState != db.DeploymentBuildLeaseStateFailed || deploymentStatus != db.DeploymentStatusFailed {
		t.Fatalf("terminal winner = lease %s deployment %s", leaseState, deploymentStatus)
	}
}
