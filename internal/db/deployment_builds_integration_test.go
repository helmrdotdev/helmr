package db_test

import (
	"context"
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
)

func TestLeaseQueuedDeploymentBuildAdvancesAndPointsDeploymentAtomically(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

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
			version, content_hash, deployment_source_artifact_id, status
		) VALUES ($1, $2, $3, $4, $5, $6, 'v2', $7, $8, 'queued')
	`, deploymentID, testPublicID(t, publicid.Deployment), ids.orgID, ids.projectID,
		ids.environmentID, dbtest.DefaultRegionID, testDigest("queued-build-"+deploymentID.String()), sourceArtifactID)

	groupID := "build-" + shortUUID(deploymentID)
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_groups (id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints)
		VALUES ($1, $2, $1, 'sha256:test-enrollment-policy', ARRAY['sha256:test-attestation'])
	`, groupID, dbtest.DefaultRegionID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, 'sha256:test-attestation', 'registering', 1, $4, now())
	`, workerID, workerID.String(), groupID, serviceID)

	leaseID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	leased, err := queries.LeaseQueuedDeploymentBuild(ctx, db.LeaseQueuedDeploymentBuildParams{
		OrgID:                       pgvalue.UUID(ids.orgID),
		DeploymentID:                pgvalue.UUID(deploymentID),
		BuildRegionID:               dbtest.DefaultRegionID,
		RequestedCpuMillis:          1000,
		RequestedMemoryBytes:        1 << 30,
		RequestedWorkloadDiskBytes:  0,
		RequestedScratchBytes:       0,
		RequestedBuildCacheBytes:    0,
		RequestedArtifactCacheBytes: 0,
		RequestedBuildExecutors:     1,
		BuildLeaseID:                pgvalue.UUID(leaseID),
		LeaseSequence:               1,
		WorkerGroupID:               groupID,
		BuildWorkerInstanceID:       pgvalue.UUID(workerID),
		WorkerEpoch:                 1,
		WorkerProtocolVersion:       "helmr.worker.v0",
		BuildSnapshot:               []byte(`{"source":"test"}`),
		StartDeadlineAt:             pgvalue.Timestamptz(now.Add(time.Minute)),
		BuildLeaseExpiresAt:         pgvalue.Timestamptz(now.Add(5 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pgvalue.MustUUIDValue(leased.ID); got != leaseID {
		t.Fatalf("lease id = %s, want %s", got, leaseID)
	}
	if leased.BuildAttemptNumber != 1 || leased.DeploymentStatus != db.DeploymentStatusBuilding {
		t.Fatalf("leased build = attempt %d status %s", leased.BuildAttemptNumber, leased.DeploymentStatus)
	}

	var status db.DeploymentStatus
	var attempt int32
	var currentLeaseID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT status, build_attempt_number, current_build_lease_id
		  FROM deployments
		 WHERE org_id = $1 AND id = $2
	`, ids.orgID, deploymentID).Scan(&status, &attempt, &currentLeaseID); err != nil {
		t.Fatal(err)
	}
	if status != db.DeploymentStatusBuilding || attempt != 1 || pgvalue.MustUUIDValue(currentLeaseID) != leaseID {
		t.Fatalf("deployment fence = status %s attempt %d lease %s", status, attempt, pgvalue.UUIDString(currentLeaseID))
	}

	mustExec(t, ctx, pool, `
		UPDATE deployment_build_leases
		   SET state = 'starting', claimed_at = now(), renewed_at = now()
		 WHERE id = $1
	`, leaseID)
	started, err := queries.StartDeploymentBuildLease(ctx, db.StartDeploymentBuildLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(now.Add(10 * time.Minute)), OrgID: pgvalue.UUID(ids.orgID),
		DeploymentID: pgvalue.UUID(deploymentID), BuildLeaseID: pgvalue.UUID(leaseID), BuildAttemptNumber: 1,
		LeaseSequence: 1, WorkerGroupID: groupID, WorkerInstanceID: pgvalue.UUID(workerID), WorkerEpoch: 1,
		RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 0, RequestedCpuMillis: 1000,
		RequestedMemoryBytes: 1 << 30, RequestedBuildExecutors: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveredStart, err := queries.GetStartedDeploymentBuildLease(ctx, db.GetStartedDeploymentBuildLeaseParams{
		OrgID: pgvalue.UUID(ids.orgID), DeploymentID: pgvalue.UUID(deploymentID), BuildLeaseID: pgvalue.UUID(leaseID),
		BuildAttemptNumber: 1, LeaseSequence: 1, WorkerGroupID: groupID, WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: "helmr.worker.v0", RequestedWorkloadDiskBytes: 0,
		RequestedScratchBytes: 0, RequestedCpuMillis: 1000, RequestedMemoryBytes: 1 << 30, RequestedBuildExecutors: 1,
	})
	if err != nil || recoveredStart.State != db.DeploymentBuildLeaseStateRunning || recoveredStart.ExpiresAt != started.ExpiresAt {
		t.Fatalf("start response-loss recovery = %+v err=%v", recoveredStart, err)
	}

	fingerprint := "sha256:7f597b648818c6c44c38b69b6198f7efee4c68f922d3a13398d64f9ff330c891"
	failure := []byte(`{"message":"deterministic failure"}`)
	failParams := db.FailDeploymentBuildParams{
		Failure: failure, ReasonCode: pgtype.Text{String: "worker_reported_failure", Valid: true},
		TerminalRequestFingerprint: fingerprint, OrgID: pgvalue.UUID(ids.orgID), ID: pgvalue.UUID(deploymentID),
		BuildLeaseID: pgvalue.UUID(leaseID), BuildWorkerInstanceID: pgvalue.UUID(workerID), WorkerEpoch: 1,
		BuildAttemptNumber: 1, LeaseSequence: 1,
	}
	if _, err := queries.FailDeploymentBuild(ctx, failParams); err != nil {
		t.Fatal(err)
	}
	terminal, err := queries.GetDeploymentBuildTerminalResult(ctx, db.GetDeploymentBuildTerminalResultParams{
		OrgID: pgvalue.UUID(ids.orgID), DeploymentID: pgvalue.UUID(deploymentID), BuildLeaseID: pgvalue.UUID(leaseID),
		BuildAttemptNumber: 1, LeaseSequence: 1, WorkerGroupID: groupID, WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: "helmr.worker.v0",
	})
	if err != nil || terminal.State != db.DeploymentBuildLeaseStateFailed || !terminal.TerminalRequestFingerprint.Valid || terminal.TerminalRequestFingerprint.String != fingerprint {
		t.Fatalf("terminal response-loss recovery = %+v err=%v", terminal, err)
	}
	if _, err := queries.FailDeploymentBuild(ctx, failParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate terminal mutation error = %v, want pgx.ErrNoRows", err)
	}
	var meterCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM meter_events WHERE deployment_build_lease_id = $1`, leaseID).Scan(&meterCount); err != nil {
		t.Fatal(err)
	}
	if meterCount != 1 {
		t.Fatalf("terminal response loss created %d meter events, want 1", meterCount)
	}
}
