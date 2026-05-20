-- name: GetRunRestorePayload :one
SELECT
    checkpoints.id AS checkpoint_id,
    checkpoints.runtime_backend,
    checkpoints.runtime_arch,
    checkpoints.runtime_abi,
    checkpoints.kernel_digest,
    checkpoints.rootfs_digest,
    checkpoints.image_key,
    checkpoints.runtime_config_digest,
    manifest_artifact.digest AS manifest_digest,
    vm_state_artifact.digest AS vm_state_digest,
    workspace_upper_artifact.digest AS workspace_upper_digest,
    COALESCE(memory_artifacts.memory_digests, '[]'::jsonb) AS memory_digests,
    checkpoints.manifest,
    waitpoints.id AS waitpoint_id,
    waitpoints.kind AS waitpoint_kind,
    waitpoints.resolution_kind,
    waitpoints.resolution
  FROM runs
  JOIN run_executions ON run_executions.org_id = runs.org_id
                      AND run_executions.run_id = runs.id
                      AND run_executions.id = runs.current_execution_id
                      AND run_executions.restore_checkpoint_id = runs.latest_checkpoint_id
  JOIN checkpoints ON checkpoints.org_id = runs.org_id
                  AND checkpoints.run_id = runs.id
                  AND checkpoints.id = runs.latest_checkpoint_id
  JOIN waitpoints ON waitpoints.org_id = runs.org_id
                 AND waitpoints.run_id = runs.id
                 AND waitpoints.checkpoint_id = checkpoints.id
  LEFT JOIN checkpoint_artifacts AS manifest_artifact
         ON manifest_artifact.org_id = checkpoints.org_id
        AND manifest_artifact.run_id = checkpoints.run_id
        AND manifest_artifact.checkpoint_id = checkpoints.id
        AND manifest_artifact.role = 'manifest'
        AND manifest_artifact.ordinal = 0
  LEFT JOIN checkpoint_artifacts AS vm_state_artifact
         ON vm_state_artifact.org_id = checkpoints.org_id
        AND vm_state_artifact.run_id = checkpoints.run_id
        AND vm_state_artifact.checkpoint_id = checkpoints.id
        AND vm_state_artifact.role = 'vm_state'
        AND vm_state_artifact.ordinal = 0
  LEFT JOIN checkpoint_artifacts AS workspace_upper_artifact
         ON workspace_upper_artifact.org_id = checkpoints.org_id
        AND workspace_upper_artifact.run_id = checkpoints.run_id
        AND workspace_upper_artifact.checkpoint_id = checkpoints.id
        AND workspace_upper_artifact.role = 'workspace_upper'
        AND workspace_upper_artifact.ordinal = 0
  LEFT JOIN LATERAL (
      SELECT jsonb_agg(checkpoint_artifacts.digest ORDER BY checkpoint_artifacts.ordinal) AS memory_digests
        FROM checkpoint_artifacts
       WHERE checkpoint_artifacts.org_id = checkpoints.org_id
         AND checkpoint_artifacts.run_id = checkpoints.run_id
         AND checkpoint_artifacts.checkpoint_id = checkpoints.id
         AND checkpoint_artifacts.role = 'memory'
  ) AS memory_artifacts ON true
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_execution_id = sqlc.arg(execution_id)
   AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
   AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
   AND runs.latest_checkpoint_id IS NOT NULL
   AND checkpoints.status = 'restoring'
   AND waitpoints.status = 'resolved'
   AND waitpoints.resolution_kind IS NOT NULL
 ORDER BY waitpoints.resolved_at DESC
 LIMIT 1;
