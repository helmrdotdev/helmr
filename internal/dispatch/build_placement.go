package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	capacityplanner "github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type placeBuildParams struct {
	Lease                   db.LeaseQueuedDeploymentBuildParams
	ExpectedLeaseSequence   int64
	ExpectedToolchainDigest []byte
}

type ReadyBuildCandidate struct {
	OrgID         pgtype.UUID
	DeploymentID  pgtype.UUID
	BuildRegionID string
	LeaseSequence int64
}

const (
	fixedBuildGuestCPUMillis   int64 = 2000
	fixedBuildGuestMemoryBytes int64 = 2 << 30
)

// PlaceReadyBuild chooses ready current-epoch build capacity in the deployment's frozen
// region. The worker never scans or chooses deployment work.
func (d *Authority) PlaceReadyBuild(ctx context.Context, candidate ReadyBuildCandidate) (db.LeaseQueuedDeploymentBuildRow, error) {
	envelope := compute.BuildEnvelopeResources()
	guest := compute.BuildGuestResources()
	requestedCPUMillis := envelope.MilliCPU
	requestedMemoryBytes := envelope.MemoryMiB << 20
	requestedGuestEphemeralDiskBytes := guest.DiskMiB << 20
	requestedBuildExecutors := int32(1)
	toolchainDigest, eligible, err := d.readyBuildCandidateToolchainDigest(ctx, candidate)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, err
	}
	if !eligible {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
	}
	if len(toolchainDigest) != 32 {
		return db.LeaseQueuedDeploymentBuildRow{}, errors.New(
			"deployment toolchain digest is invalid",
		)
	}
	bins, err := db.New(d.pool).ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		RegionID: candidate.BuildRegionID, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
	})
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("discover build worker capacity: %w", err)
	}
	worker, ok := capacityplanner.SelectBuildWorker(bins)
	if !ok || !worker.WorkerEpoch.Valid {
		eligible, checkErr := d.readyBuildCandidateExists(ctx, candidate)
		if checkErr != nil {
			return db.LeaseQueuedDeploymentBuildRow{}, checkErr
		}
		if !eligible {
			return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
		}
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCapacityUnavailable
	}
	now := time.Now().UTC()
	snapshot, _ := json.Marshal(map[string]any{"source": "dispatcher", "lease_sequence": candidate.LeaseSequence})
	row, err := d.placeBuild(ctx, placeBuildParams{
		ExpectedLeaseSequence:   candidate.LeaseSequence,
		ExpectedToolchainDigest: toolchainDigest,
		Lease: db.LeaseQueuedDeploymentBuildParams{
			OrgID: candidate.OrgID, DeploymentID: candidate.DeploymentID,
			BuildRegionID: candidate.BuildRegionID,
			BuildLeaseID:  pgvalue.UUID(uuid.Must(uuid.NewV7())), LeaseSequence: candidate.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, BuildWorkerInstanceID: worker.WorkerInstanceID,
			WorkerEpoch: worker.WorkerEpoch.Int64, RequestedCPUMillis: requestedCPUMillis,
			RequestedMemoryBytes:             requestedMemoryBytes,
			RequestedGuestEphemeralDiskBytes: requestedGuestEphemeralDiskBytes,
			RequestedBuildExecutors:          requestedBuildExecutors, BuildSnapshot: snapshot,
			StartDeadlineAt:     pgvalue.Timestamptz(now.Add(time.Minute)),
			BuildLeaseExpiresAt: pgvalue.Timestamptz(now.Add(5 * time.Minute)),
		},
	})
	if err != nil && (errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrCandidateChanged)) {
		eligible, checkErr := d.readyBuildCandidateExists(ctx, candidate)
		if checkErr != nil {
			return db.LeaseQueuedDeploymentBuildRow{}, checkErr
		}
		if !eligible {
			return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
		}
	}
	return row, err
}

func (d *Authority) readyBuildCandidateExists(ctx context.Context, candidate ReadyBuildCandidate) (bool, error) {
	_, exists, err := d.readyBuildCandidateToolchainDigest(ctx, candidate)
	return exists, err
}

func (d *Authority) readyBuildCandidateToolchainDigest(
	ctx context.Context,
	candidate ReadyBuildCandidate,
) ([]byte, bool, error) {
	var digest []byte
	err := d.pool.QueryRow(ctx, `
SELECT build_toolchain_digest
  FROM deployments
  WHERE org_id = $1 AND id = $2 AND build_region_id = $3
    AND status IN ('queued','building')
    AND build_runtime_digest IS NOT NULL
    AND build_toolchain_digest IS NOT NULL
    AND build_manager_digest IS NOT NULL
    AND (COALESCE((SELECT max(lease_sequence) FROM deployment_build_leases
                   WHERE deployment_id = deployments.id), 0) + 1) = $4
    AND $4 BETWEEN 1 AND 3
    AND NOT EXISTS (SELECT 1 FROM deployment_build_leases
                     WHERE deployment_id = deployments.id AND state IN ('assigned','starting','running'))
`, candidate.OrgID, candidate.DeploymentID, candidate.BuildRegionID,
		candidate.LeaseSequence).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("revalidate ready build candidate: %w", err)
	}
	return digest, true, nil
}

func (d *Authority) placeBuild(ctx context.Context, params placeBuildParams) (db.LeaseQueuedDeploymentBuildRow, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("begin build placement: %w", err)
	}
	defer rollback(ctx, tx)

	var candidateID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT deployments.id
 FROM deployments
 WHERE deployments.id = $2
   AND deployments.status IN ('queued', 'building')
   AND deployments.build_region_id = $1
   AND deployments.build_toolchain_digest = $3
   AND NOT EXISTS (
       SELECT 1 FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
          AND deployment_build_leases.state IN ('assigned', 'starting', 'running'))
 ORDER BY deployments.created_at, deployments.id
LIMIT 1`, params.Lease.BuildRegionID, params.Lease.DeploymentID,
		params.ExpectedToolchainDigest).Scan(&candidateID)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("discover build placement candidate: %w", err)
	}
	if err := lockSource(ctx, tx, "deployment", candidateID); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, err
	}

	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID: params.Lease.WorkerGroupID, RegionID: params.Lease.BuildRegionID,
		WorkerInstanceID: params.Lease.BuildWorkerInstanceID, WorkerEpoch: params.Lease.WorkerEpoch,
		Role: "build",
	}); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, err
	}
	var deploymentFenceMatches bool
	if err := tx.QueryRow(ctx, `
SELECT (COALESCE((SELECT max(lease_sequence) FROM deployment_build_leases
                  WHERE deployment_id = deployments.id), 0) + 1) = $3
   AND $3 BETWEEN 1 AND 3
 FROM deployments
 WHERE id = $1 AND build_region_id = $2
   AND build_toolchain_digest = $4
   AND status IN ('queued','building')
 FOR UPDATE`, candidateID, params.Lease.BuildRegionID,
		params.ExpectedLeaseSequence,
		params.ExpectedToolchainDigest).Scan(&deploymentFenceMatches); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("lock build deployment: %w", err)
	}
	if !deploymentFenceMatches {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
	}
	var hasCapacity bool
	err = tx.QueryRow(ctx, `
SELECT worker_instances.max_build_executors >=
	           COALESCE(sum(deployment_build_leases.requested_build_executors), 0) + $3
	   AND worker_instances.per_vm_cpu_millis >= $7
	   AND worker_instances.per_vm_memory_bytes >= $8
	   AND worker_instances.per_vm_guest_ephemeral_disk_bytes >= $6
   AND worker_instances.epoch_cpu_millis >=
           COALESCE(sum(deployment_build_leases.requested_cpu_millis), 0)
           + COALESCE((SELECT sum(reserved_cpu_millis) FROM runtime_instances
                        WHERE worker_instance_id = worker_instances.id
                          AND worker_epoch = worker_instances.current_epoch
                          AND (observed_state IN ('allocated','preparing','ready','closing')
                            OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0) + $4
   AND worker_instances.epoch_memory_bytes >=
           COALESCE(sum(deployment_build_leases.requested_memory_bytes), 0)
           + COALESCE((SELECT sum(reserved_memory_bytes) FROM runtime_instances
                        WHERE worker_instance_id = worker_instances.id
                          AND worker_epoch = worker_instances.current_epoch
                          AND (observed_state IN ('allocated','preparing','ready','closing')
                            OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0) + $5
   AND worker_instances.epoch_guest_ephemeral_disk_bytes >=
           COALESCE(sum(deployment_build_leases.requested_guest_ephemeral_disk_bytes), 0)
           + COALESCE((SELECT sum(reserved_guest_ephemeral_disk_bytes) FROM runtime_instances
                        WHERE worker_instance_id = worker_instances.id
                          AND worker_epoch = worker_instances.current_epoch
                          AND (observed_state IN ('allocated','preparing','ready','closing')
                            OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0) + $6
  FROM worker_instances
  LEFT JOIN deployment_build_leases
    ON deployment_build_leases.worker_instance_id = worker_instances.id
   AND deployment_build_leases.worker_epoch = worker_instances.current_epoch
   AND deployment_build_leases.state IN ('assigned','starting','running')
 WHERE worker_instances.id = $1 AND worker_instances.current_epoch = $2
 GROUP BY worker_instances.id,
          worker_instances.current_epoch,
          worker_instances.max_build_executors,
          worker_instances.epoch_cpu_millis,
          worker_instances.epoch_memory_bytes,
          worker_instances.epoch_guest_ephemeral_disk_bytes,
	          worker_instances.per_vm_cpu_millis,
	          worker_instances.per_vm_memory_bytes,
	          worker_instances.per_vm_guest_ephemeral_disk_bytes`,
		params.Lease.BuildWorkerInstanceID, params.Lease.WorkerEpoch,
		params.Lease.RequestedBuildExecutors, params.Lease.RequestedCPUMillis,
		params.Lease.RequestedMemoryBytes, params.Lease.RequestedGuestEphemeralDiskBytes,
		fixedBuildGuestCPUMillis, fixedBuildGuestMemoryBytes).Scan(&hasCapacity)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("check build capacity: %w", err)
	}
	if !hasCapacity {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCapacityUnavailable
	}

	row, err := db.New(tx).LeaseQueuedDeploymentBuild(ctx, params.Lease)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("insert build lease: %w", err)
	}
	if row.DeploymentID != candidateID {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
	}
	if err := tx.Commit(ctx); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("commit build placement: %w", err)
	}
	return row, nil
}
