package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	buildCPU     int64 = 2000
	buildMemory  int64 = 2 << 30
	buildScratch int64 = 13 << 30
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
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)

	var sourceArtifactID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT deployment_source_artifact_id
		  FROM deployments
		 WHERE org_id = $1 AND id = $2
	`, ids.orgID, ids.deploymentID).Scan(&sourceArtifactID); err != nil {
		t.Fatal(err)
	}

	deploymentID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO deployments (
			id, public_id, org_id, project_id, environment_id, build_region_id,
			build_architecture, build_runtime_digest, build_standard_toolchain_digest,
			build_materializer_version,
			version, content_hash, deployment_source_artifact_id, status
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			'x86_64', decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
			'helmr.dependencies.v0',
			$7, $8, $9, 'queued'
		)
	`, deploymentID, testPublicID(t, publicid.Deployment), ids.orgID, ids.projectID,
		ids.environmentID, dbtest.DefaultRegionID, "build-"+shortUUID(deploymentID),
		testDigest("deployment-build-"+deploymentID.String()), sourceArtifactID)

	groupID := "build-" + shortUUID(deploymentID)
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_groups (
			id, region_id, name, enrollment_policy_fingerprint,
			allowed_attestation_fingerprints
		) VALUES ($1, $2, $1, 'sha256:test-enrollment-policy', ARRAY['sha256:test-attestation'])
	`, groupID, dbtest.DefaultRegionID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, 'sha256:test-attestation', 'registering', 1, $4, now())
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

func (f *deploymentBuildFixture) lease(t *testing.T, sequence int64) db.LeaseQueuedDeploymentBuildRow {
	t.Helper()
	now := time.Now().UTC()
	leaseID := uuid.Must(uuid.NewV7())
	row, err := f.queries.LeaseQueuedDeploymentBuild(f.ctx, db.LeaseQueuedDeploymentBuildParams{
		OrgID:                      pgvalue.UUID(f.orgID),
		DeploymentID:               pgvalue.UUID(f.deploymentID),
		BuildRegionID:              dbtest.DefaultRegionID,
		RequestedCpuMillis:         buildCPU,
		RequestedMemoryBytes:       buildMemory,
		RequestedWorkloadDiskBytes: 0,
		RequestedScratchBytes:      buildScratch,
		RequestedBuildExecutors:    1,
		BuildLeaseID:               pgvalue.UUID(leaseID),
		LeaseSequence:              sequence,
		WorkerGroupID:              f.groupID,
		BuildWorkerInstanceID:      pgvalue.UUID(f.workerID),
		WorkerEpoch:                1,
		WorkerProtocolVersion:      "helmr.worker.v0",
		BuildSnapshot:              []byte(`{"source":"test"}`),
		StartDeadlineAt:            pgvalue.Timestamptz(now.Add(time.Minute)),
		BuildLeaseExpiresAt:        pgvalue.Timestamptz(now.Add(5 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *deploymentBuildFixture) start(t *testing.T, leaseID uuid.UUID, sequence int64) db.DeploymentBuildLease {
	t.Helper()
	mustExec(t, f.ctx, f.pool, `
		UPDATE deployment_build_leases
		   SET state = 'starting', claimed_at = now(), renewed_at = now()
		 WHERE id = $1
	`, leaseID)
	row, err := f.queries.StartDeploymentBuildLease(f.ctx, db.StartDeploymentBuildLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(time.Now().UTC().Add(10 * time.Minute)),
		OrgID:     pgvalue.UUID(f.orgID), DeploymentID: pgvalue.UUID(f.deploymentID),
		BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: sequence,
		WorkerGroupID: f.groupID, WorkerInstanceID: pgvalue.UUID(f.workerID),
		WorkerEpoch: 1, RequestedWorkloadDiskBytes: 0,
		RequestedScratchBytes: buildScratch, RequestedCpuMillis: buildCPU,
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
	var artifactID pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, `
		SELECT deployment_source_artifact_id
		  FROM deployments
		 WHERE org_id = $1 AND id = $2
	`, f.orgID, f.deploymentID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	row, err := f.queries.CompleteDeploymentBuild(f.ctx, db.CompleteDeploymentBuildParams{
		TerminalRequestFingerprint:   "sha256:complete-" + leaseID.String(),
		OrgID:                        pgvalue.UUID(f.orgID),
		ID:                           pgvalue.UUID(f.deploymentID),
		BuildLeaseID:                 pgvalue.UUID(leaseID),
		BuildWorkerInstanceID:        pgvalue.UUID(f.workerID),
		WorkerEpoch:                  1,
		LeaseSequence:                sequence,
		BuildManifestArtifactID:      artifactID,
		DeploymentManifestArtifactID: artifactID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestDeploymentBuildDeliveryRedrivesAndExhausts(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)

	var cpu, memory, scratch int64
	if err := pool.QueryRow(f.ctx, `
		SELECT build_requested_cpu_millis,
		       build_requested_memory_bytes,
		       build_requested_scratch_bytes
		  FROM deployments
		 WHERE id = $1
	`, f.deploymentID).Scan(&cpu, &memory, &scratch); err != nil {
		t.Fatal(err)
	}
	if cpu != buildCPU || memory != buildMemory || scratch != buildScratch {
		t.Fatalf("stored build reserve = (%d,%d,%d)", cpu, memory, scratch)
	}

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

func TestExpiredDeploymentBuildDeliveryUsesTheSameBoundedPolicy(t *testing.T) {
	f, pool := newDeploymentBuildFixture(t)

	first := f.lease(t, 1)
	firstID := pgvalue.MustUUIDValue(first.ID)
	f.start(t, firstID, 1)
	mustExec(t, f.ctx, pool, `
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
	mustExec(t, f.ctx, pool, `
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
		RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: buildScratch,
		RequestedCpuMillis: buildCPU, RequestedMemoryBytes: buildMemory,
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
	mustExec(t, f.ctx, pool, `
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
