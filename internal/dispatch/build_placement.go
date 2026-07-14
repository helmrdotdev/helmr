package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type placeBuildParams struct {
	Lease                      db.LeaseQueuedDeploymentBuildParams
	ObservationFreshAfter      pgtype.Timestamptz
	ExpectedBuildAttemptNumber int32
	ExpectedLeaseSequence      int64
}

type ReadyBuildCandidate struct {
	OrgID                       pgtype.UUID
	DeploymentID                pgtype.UUID
	BuildRegionID               string
	BuildAttemptNumber          int32
	LeaseSequence               int64
	RequestedCPUMillis          int64
	RequestedMemoryBytes        int64
	RequestedWorkloadDiskBytes  int64
	RequestedScratchBytes       int64
	RequestedBuildCacheBytes    int64
	RequestedArtifactCacheBytes int64
	RequestedBuildExecutors     int32
}

// PlaceReadyBuild chooses certified build capacity in the deployment's frozen
// region. The worker never scans or chooses deployment work.
func (d *Authority) PlaceReadyBuild(ctx context.Context, candidate ReadyBuildCandidate, observationFreshAfter pgtype.Timestamptz) (db.LeaseQueuedDeploymentBuildRow, error) {
	eligible, err := d.readyBuildCandidateExists(ctx, candidate)
	if err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, err
	}
	if !eligible {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
	}
	var groupID, protocolVersion string
	var workerID pgtype.UUID
	var workerEpoch int64
	err = d.pool.QueryRow(ctx, `
SELECT worker_groups.id, worker_instances.id, worker_instances.current_epoch,
       worker_instances.protocol_version
  FROM worker_groups
  JOIN worker_instances ON worker_instances.worker_group_id = worker_groups.id
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
   AND worker_instances.protocol_version = worker_groups.protocol_version
   AND worker_observations.observed_at >= $2
	   AND worker_observations.build_paused_reason IS NULL
	   AND $4 <= worker_instances.per_vm_cpu_millis * $3
	   AND $5 <= worker_instances.per_vm_memory_bytes * $3
	   AND $6 <= worker_instances.per_vm_workload_disk_bytes * $3
	   AND $7 <= worker_instances.per_vm_scratch_bytes * $3
	GROUP BY worker_groups.id, worker_instances.id, worker_instances.current_epoch,
	         worker_instances.protocol_version, worker_instances.certified_cpu_millis,
	         worker_instances.certified_memory_bytes,
	         worker_instances.certified_workload_disk_bytes,
	         worker_instances.certified_scratch_bytes,
	         worker_instances.certified_build_cache_bytes,
		         worker_instances.certified_artifact_cache_bytes,
		         worker_instances.per_vm_cpu_millis, worker_instances.per_vm_memory_bytes,
		         worker_instances.per_vm_workload_disk_bytes, worker_instances.per_vm_scratch_bytes,
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
    AND worker_instances.certified_build_cache_bytes
          - COALESCE(sum(deployment_build_leases.requested_build_cache_bytes),0) >= $8
    AND worker_instances.certified_artifact_cache_bytes
          - COALESCE(sum(deployment_build_leases.requested_artifact_cache_bytes),0) >= $9
 ORDER BY worker_instances.id
LIMIT 1`, candidate.BuildRegionID, observationFreshAfter, candidate.RequestedBuildExecutors,
		candidate.RequestedCPUMillis, candidate.RequestedMemoryBytes,
		candidate.RequestedWorkloadDiskBytes, candidate.RequestedScratchBytes,
		candidate.RequestedBuildCacheBytes, candidate.RequestedArtifactCacheBytes).Scan(
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
	snapshot, _ := json.Marshal(map[string]any{"source": "dispatcher", "build_attempt_number": candidate.BuildAttemptNumber, "lease_sequence": candidate.LeaseSequence})
	row, err := d.placeBuild(ctx, placeBuildParams{
		ObservationFreshAfter:      observationFreshAfter,
		ExpectedBuildAttemptNumber: candidate.BuildAttemptNumber,
		ExpectedLeaseSequence:      candidate.LeaseSequence,
		Lease: db.LeaseQueuedDeploymentBuildParams{
			OrgID: candidate.OrgID, DeploymentID: candidate.DeploymentID,
			BuildRegionID: candidate.BuildRegionID,
			BuildLeaseID:  pgvalue.UUID(uuid.Must(uuid.NewV7())), LeaseSequence: candidate.LeaseSequence,
			WorkerGroupID: groupID, BuildWorkerInstanceID: workerID,
			WorkerEpoch: workerEpoch, WorkerProtocolVersion: protocolVersion,
			RequestedCpuMillis:          candidate.RequestedCPUMillis,
			RequestedMemoryBytes:        candidate.RequestedMemoryBytes,
			RequestedWorkloadDiskBytes:  candidate.RequestedWorkloadDiskBytes,
			RequestedScratchBytes:       candidate.RequestedScratchBytes,
			RequestedBuildCacheBytes:    candidate.RequestedBuildCacheBytes,
			RequestedArtifactCacheBytes: candidate.RequestedArtifactCacheBytes,
			RequestedBuildExecutors:     candidate.RequestedBuildExecutors, BuildSnapshot: snapshot,
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
	var exists bool
	err := d.pool.QueryRow(ctx, `
SELECT EXISTS (
 SELECT 1 FROM deployments
  WHERE org_id = $1 AND id = $2 AND build_region_id = $3
    AND status IN ('queued','building')
    AND build_requested_cpu_millis = $4
    AND build_requested_memory_bytes = $5
    AND build_requested_workload_disk_bytes = $6
    AND build_requested_scratch_bytes = $7
    AND build_requested_build_cache_bytes = $8
    AND build_requested_artifact_cache_bytes = $9
    AND build_requested_executors = $10
    AND (CASE WHEN status = 'queued' THEN build_attempt_number + 1 ELSE build_attempt_number END) = $11
    AND (COALESCE((SELECT max(lease_sequence) FROM deployment_build_leases
                   WHERE deployment_id = deployments.id AND build_attempt_number = $11), 0) + 1) = $12
    AND NOT EXISTS (SELECT 1 FROM deployment_build_leases
                     WHERE deployment_id = deployments.id AND state IN ('assigned','starting','running'))
)`, candidate.OrgID, candidate.DeploymentID, candidate.BuildRegionID,
		candidate.RequestedCPUMillis, candidate.RequestedMemoryBytes, candidate.RequestedWorkloadDiskBytes,
		candidate.RequestedScratchBytes, candidate.RequestedBuildCacheBytes, candidate.RequestedArtifactCacheBytes,
		candidate.RequestedBuildExecutors, candidate.BuildAttemptNumber, candidate.LeaseSequence).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("revalidate ready build candidate: %w", err)
	}
	return exists, nil
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
   AND NOT EXISTS (
       SELECT 1 FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
          AND deployment_build_leases.state IN ('assigned', 'starting', 'running'))
 ORDER BY deployments.created_at, deployments.id
LIMIT 1`, params.Lease.BuildRegionID, params.Lease.DeploymentID).Scan(&candidateID)
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
	}); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, err
	}
	var deploymentFenceMatches bool
	if err := tx.QueryRow(ctx, `
SELECT (CASE WHEN status = 'queued' THEN build_attempt_number + 1 ELSE build_attempt_number END) = $3
   AND (COALESCE((SELECT max(lease_sequence) FROM deployment_build_leases
                  WHERE deployment_id = deployments.id AND build_attempt_number = $3), 0) + 1) = $4
  FROM deployments
 WHERE id = $1 AND build_region_id = $2 AND status IN ('queued','building')
 FOR UPDATE`, candidateID, params.Lease.BuildRegionID, params.ExpectedBuildAttemptNumber,
		params.ExpectedLeaseSequence).Scan(&deploymentFenceMatches); err != nil {
		return db.LeaseQueuedDeploymentBuildRow{}, fmt.Errorf("lock build deployment: %w", err)
	}
	if !deploymentFenceMatches {
		return db.LeaseQueuedDeploymentBuildRow{}, ErrCandidateChanged
	}
	var hasCapacity bool
	err = tx.QueryRow(ctx, `
SELECT worker_instances.max_build_executors >=
	           COALESCE(sum(deployment_build_leases.requested_build_executors), 0) + $3
	   AND $4 <= worker_instances.per_vm_cpu_millis * $3
	   AND $5 <= worker_instances.per_vm_memory_bytes * $3
	   AND $6 <= worker_instances.per_vm_workload_disk_bytes * $3
	   AND $7 <= worker_instances.per_vm_scratch_bytes * $3
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
   AND worker_instances.certified_build_cache_bytes >=
           COALESCE(sum(deployment_build_leases.requested_build_cache_bytes), 0) + $8
   AND worker_instances.certified_artifact_cache_bytes >=
           COALESCE(sum(deployment_build_leases.requested_artifact_cache_bytes), 0) + $9
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
	          worker_instances.certified_build_cache_bytes,
	          worker_instances.certified_artifact_cache_bytes,
	          worker_instances.per_vm_cpu_millis,
	          worker_instances.per_vm_memory_bytes,
	          worker_instances.per_vm_workload_disk_bytes,
	          worker_instances.per_vm_scratch_bytes`,
		params.Lease.BuildWorkerInstanceID, params.Lease.WorkerEpoch,
		params.Lease.RequestedBuildExecutors, params.Lease.RequestedCpuMillis,
		params.Lease.RequestedMemoryBytes, params.Lease.RequestedWorkloadDiskBytes,
		params.Lease.RequestedScratchBytes, params.Lease.RequestedBuildCacheBytes,
		params.Lease.RequestedArtifactCacheBytes).Scan(&hasCapacity)
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
