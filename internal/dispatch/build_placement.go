package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type placeBuildParams struct {
	Lease                           db.LeaseQueuedDeploymentBuildParams
	ObservationFreshAfter           pgtype.Timestamptz
	ExpectedLeaseSequence           int64
	ExpectedStandardToolchainDigest []byte
}

type ReadyBuildCandidate struct {
	OrgID         pgtype.UUID
	DeploymentID  pgtype.UUID
	BuildRegionID string
	LeaseSequence int64
}

const (
	fixedBuildGuestCPUMillis    int64 = 2000
	fixedBuildGuestMemoryBytes  int64 = 2 << 30
	fixedBuildGuestScratchBytes int64 = 8 << 30
)

// PlaceReadyBuild chooses certified build capacity in the deployment's frozen
// region. The worker never scans or chooses deployment work.
func (d *Authority) PlaceReadyBuild(ctx context.Context, candidate ReadyBuildCandidate, observationFreshAfter pgtype.Timestamptz) (db.LeaseQueuedDeploymentBuildRow, error) {
	if d.resolveBuildToolchain == nil || len(d.toolchainCatalogDigest) != 32 {
		return db.LeaseQueuedDeploymentBuildRow{}, errors.New(
			"build authority standard-toolchain catalog is not configured",
		)
	}
	envelope := compute.BuildEnvelopeResources()
	requestedCPUMillis := envelope.MilliCPU
	requestedMemoryBytes := envelope.MemoryMiB << 20
	requestedWorkloadDiskBytes := int64(0)
	requestedScratchBytes := envelope.DiskMiB << 20
	requestedBuildExecutors := int32(1)
	standardToolchainDigest, eligible, err := d.readyBuildCandidateToolchainDigest(ctx, candidate)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, err
	}
	if !eligible {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
	}
	if len(standardToolchainDigest) != 32 {
		return db.LeaseQueuedDeploymentBuildRow{}, errors.New(
			"deployment standard toolchain digest is invalid",
		)
	}
	standardToolchainDigestString := fmt.Sprintf("sha256:%x", standardToolchainDigest)
	if err := d.resolveBuildToolchain(standardToolchainDigestString); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCapacityUnavailable
	}
	var groupID, protocolVersion string
	var workerID pgtype.UUID
	var workerEpoch int64
	err = d.pool.QueryRow(ctx, `
SELECT worker_groups.id, worker_instances.id, worker_instances.current_epoch,
       worker_instances.protocol_version
  FROM worker_groups
  JOIN worker_instances ON worker_instances.worker_group_id = worker_groups.id
  JOIN runtime_identities
    ON runtime_identities.id = worker_instances.runtime_identity_id
   AND runtime_identities.runtime_arch = 'x86_64'
  JOIN worker_observations
    ON worker_observations.worker_instance_id = worker_instances.id
   AND worker_observations.worker_epoch = worker_instances.current_epoch
  LEFT JOIN deployment_build_leases
    ON deployment_build_leases.worker_instance_id = worker_instances.id
   AND deployment_build_leases.worker_epoch = worker_instances.current_epoch
   AND deployment_build_leases.state IN ('assigned','starting','running')
 WHERE worker_groups.region_id = $1 AND worker_groups.state = 'active'
   AND worker_groups.allows_build
   AND worker_instances.state = 'active' AND worker_instances.supports_build
   AND worker_instances.toolchain_catalog_digest = $11
   AND worker_instances.certified_at IS NOT NULL
   AND worker_instances.protocol_version = worker_groups.protocol_version
   AND worker_observations.observed_at >= $2
	   AND worker_observations.build_paused_reason IS NULL
	   AND worker_instances.per_vm_cpu_millis >= $8
	   AND worker_instances.per_vm_memory_bytes >= $9
	   AND worker_instances.per_vm_scratch_bytes >= $10
	GROUP BY worker_groups.id, worker_instances.id, worker_instances.current_epoch,
	         worker_instances.protocol_version, worker_instances.certified_cpu_millis,
	         worker_instances.certified_memory_bytes,
	         worker_instances.certified_workload_disk_bytes,
	         worker_instances.certified_scratch_bytes,
		         worker_instances.per_vm_cpu_millis, worker_instances.per_vm_memory_bytes,
		         worker_instances.per_vm_scratch_bytes,
	         worker_instances.max_build_executors
 HAVING COALESCE(sum(deployment_build_leases.requested_build_executors),0) + $3
          <= worker_instances.max_build_executors
    AND worker_instances.certified_cpu_millis
          - COALESCE(sum(deployment_build_leases.requested_cpu_millis),0)
          - COALESCE((SELECT sum(reserved_cpu_millis) FROM runtime_instances
                       WHERE worker_instance_id = worker_instances.id
                         AND worker_epoch = worker_instances.current_epoch
                         AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))),0) >= $4
    AND worker_instances.certified_memory_bytes
          - COALESCE(sum(deployment_build_leases.requested_memory_bytes),0)
          - COALESCE((SELECT sum(reserved_memory_bytes) FROM runtime_instances
                       WHERE worker_instance_id = worker_instances.id
                         AND worker_epoch = worker_instances.current_epoch
                         AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))),0) >= $5
    AND worker_instances.certified_workload_disk_bytes
          - COALESCE(sum(deployment_build_leases.requested_workload_disk_bytes),0)
          - COALESCE((SELECT sum(reserved_workload_disk_bytes) FROM runtime_instances
                       WHERE worker_instance_id = worker_instances.id
                         AND worker_epoch = worker_instances.current_epoch
                         AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))),0) >= $6
    AND worker_instances.certified_scratch_bytes
          - COALESCE(sum(deployment_build_leases.requested_scratch_bytes),0)
          - COALESCE((SELECT sum(reserved_scratch_bytes) FROM runtime_instances
                       WHERE worker_instance_id = worker_instances.id
                         AND worker_epoch = worker_instances.current_epoch
                         AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))),0) >= $7
 ORDER BY worker_instances.id
LIMIT 1`, candidate.BuildRegionID, observationFreshAfter, requestedBuildExecutors,
		requestedCPUMillis, requestedMemoryBytes,
		requestedWorkloadDiskBytes, requestedScratchBytes,
		fixedBuildGuestCPUMillis, fixedBuildGuestMemoryBytes, fixedBuildGuestScratchBytes,
		d.toolchainCatalogDigest).Scan(
		&groupID, &workerID, &workerEpoch, &protocolVersion)
	if err != nil {
		if err == pgx.ErrNoRows {
			eligible, checkErr := d.readyBuildCandidateExists(ctx, candidate)
			if checkErr != nil {
				return db.LeaseQueuedDeploymentBuildRow{}, checkErr
			}
			if !eligible {
				return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
			}
			return db.LeaseQueuedDeploymentBuildRow{}, ErrCapacityUnavailable
		}
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("discover build worker: %w", err)
	}
	now := time.Now().UTC()
	snapshot, _ := json.Marshal(map[string]any{"source": "dispatcher", "lease_sequence": candidate.LeaseSequence})
	row, err := d.placeBuild(ctx, placeBuildParams{
		ObservationFreshAfter:           observationFreshAfter,
		ExpectedLeaseSequence:           candidate.LeaseSequence,
		ExpectedStandardToolchainDigest: standardToolchainDigest,
		Lease: db.LeaseQueuedDeploymentBuildParams{
			OrgID: candidate.OrgID, DeploymentID: candidate.DeploymentID,
			BuildRegionID: candidate.BuildRegionID,
			BuildLeaseID:  pgvalue.UUID(uuid.Must(uuid.NewV7())), LeaseSequence: candidate.LeaseSequence,
			WorkerGroupID: groupID, BuildWorkerInstanceID: workerID,
			WorkerEpoch: workerEpoch, WorkerProtocolVersion: protocolVersion,
			RequestedCpuMillis:         requestedCPUMillis,
			RequestedMemoryBytes:       requestedMemoryBytes,
			RequestedWorkloadDiskBytes: requestedWorkloadDiskBytes,
			RequestedScratchBytes:      requestedScratchBytes,
			RequestedBuildExecutors:    requestedBuildExecutors, BuildSnapshot: snapshot,
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
SELECT build_standard_toolchain_digest
  FROM deployments
  WHERE org_id = $1 AND id = $2 AND build_region_id = $3
    AND status IN ('queued','building')
    AND build_runtime_digest IS NOT NULL
    AND build_standard_toolchain_digest IS NOT NULL
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
   AND deployments.build_standard_toolchain_digest = $3
   AND NOT EXISTS (
       SELECT 1 FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
          AND deployment_build_leases.state IN ('assigned', 'starting', 'running'))
 ORDER BY deployments.created_at, deployments.id
LIMIT 1`, params.Lease.BuildRegionID, params.Lease.DeploymentID,
		params.ExpectedStandardToolchainDigest).Scan(&candidateID)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("discover build placement candidate: %w", err)
	}
	if err := lockSource(ctx, tx, "deployment", candidateID); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, err
	}

	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID: params.Lease.WorkerGroupID, RegionID: params.Lease.BuildRegionID,
		WorkerInstanceID: params.Lease.BuildWorkerInstanceID, WorkerEpoch: params.Lease.WorkerEpoch,
		WorkerProtocolVersion: params.Lease.WorkerProtocolVersion,
		ObservationFreshAfter: params.ObservationFreshAfter, Role: "build",
		ToolchainCatalogDigest: d.toolchainCatalogDigest,
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
   AND build_standard_toolchain_digest = $4
   AND status IN ('queued','building')
 FOR UPDATE`, candidateID, params.Lease.BuildRegionID,
		params.ExpectedLeaseSequence,
		params.ExpectedStandardToolchainDigest).Scan(&deploymentFenceMatches); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("lock build deployment: %w", err)
	}
	if !deploymentFenceMatches {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
	}
	var hasCapacity bool
	err = tx.QueryRow(ctx, `
SELECT worker_instances.max_build_executors >=
	           COALESCE(sum(deployment_build_leases.requested_build_executors), 0) + $3
	   AND worker_instances.per_vm_cpu_millis >= $8
	   AND worker_instances.per_vm_memory_bytes >= $9
	   AND worker_instances.per_vm_scratch_bytes >= $10
   AND worker_instances.certified_cpu_millis >=
           COALESCE(sum(deployment_build_leases.requested_cpu_millis), 0)
           + COALESCE((SELECT sum(reserved_cpu_millis) FROM runtime_instances
                        WHERE worker_instance_id = worker_instances.id
                          AND worker_epoch = worker_instances.current_epoch
                          AND (observed_state IN ('allocated','preparing','ready','closing')
                            OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0) + $4
   AND worker_instances.certified_memory_bytes >=
           COALESCE(sum(deployment_build_leases.requested_memory_bytes), 0)
           + COALESCE((SELECT sum(reserved_memory_bytes) FROM runtime_instances
                        WHERE worker_instance_id = worker_instances.id
                          AND worker_epoch = worker_instances.current_epoch
                          AND (observed_state IN ('allocated','preparing','ready','closing')
                            OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0) + $5
   AND worker_instances.certified_workload_disk_bytes >=
           COALESCE(sum(deployment_build_leases.requested_workload_disk_bytes), 0)
           + COALESCE((SELECT sum(reserved_workload_disk_bytes) FROM runtime_instances
                        WHERE worker_instance_id = worker_instances.id
                          AND worker_epoch = worker_instances.current_epoch
                          AND (observed_state IN ('allocated','preparing','ready','closing')
                            OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0) + $6
   AND worker_instances.certified_scratch_bytes >=
           COALESCE(sum(deployment_build_leases.requested_scratch_bytes), 0)
           + COALESCE((SELECT sum(reserved_scratch_bytes) FROM runtime_instances
                        WHERE worker_instance_id = worker_instances.id
                          AND worker_epoch = worker_instances.current_epoch
                          AND (observed_state IN ('allocated','preparing','ready','closing')
                            OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0) + $7
  FROM worker_instances
  LEFT JOIN deployment_build_leases
    ON deployment_build_leases.worker_instance_id = worker_instances.id
   AND deployment_build_leases.worker_epoch = worker_instances.current_epoch
   AND deployment_build_leases.state IN ('assigned','starting','running')
 WHERE worker_instances.id = $1 AND worker_instances.current_epoch = $2
 GROUP BY worker_instances.id,
          worker_instances.current_epoch,
          worker_instances.max_build_executors,
          worker_instances.certified_cpu_millis,
          worker_instances.certified_memory_bytes,
          worker_instances.certified_workload_disk_bytes,
          worker_instances.certified_scratch_bytes,
	          worker_instances.per_vm_cpu_millis,
	          worker_instances.per_vm_memory_bytes,
	          worker_instances.per_vm_scratch_bytes`,
		params.Lease.BuildWorkerInstanceID, params.Lease.WorkerEpoch,
		params.Lease.RequestedBuildExecutors, params.Lease.RequestedCpuMillis,
		params.Lease.RequestedMemoryBytes, params.Lease.RequestedWorkloadDiskBytes,
		params.Lease.RequestedScratchBytes, fixedBuildGuestCPUMillis,
		fixedBuildGuestMemoryBytes, fixedBuildGuestScratchBytes).Scan(&hasCapacity)
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
