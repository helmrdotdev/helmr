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
    checkpoints.workspace_base_kind,
    checkpoints.workspace_repository,
    checkpoints.workspace_ref,
    checkpoints.workspace_sha,
    checkpoints.workspace_subpath,
    checkpoints.workspace_ref_kind,
    checkpoints.workspace_ref_name,
    checkpoints.workspace_full_ref,
    checkpoints.workspace_default_branch,
    checkpoints.workspace_artifact_digest,
    checkpoints.workspace_artifact_media_type,
    checkpoints.workspace_artifact_encoding,
    checkpoints.workspace_mount_path,
    checkpoints.workspace_project_subpath,
    checkpoints.workspace_volume_kind,
    COALESCE(checkpoint_artifacts.artifacts, '[]'::jsonb) AS checkpoint_artifacts,
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
  LEFT JOIN LATERAL (
      SELECT jsonb_agg(
                 jsonb_build_object(
                     'role', checkpoint_artifacts.role,
                     'ordinal', checkpoint_artifacts.ordinal,
                     'digest', checkpoint_artifacts.digest,
                     'size_bytes', checkpoint_artifacts.size_bytes,
                     'media_type', checkpoint_artifacts.media_type,
                     'encrypt_duration_ms', checkpoint_artifacts.encrypt_duration_ms,
                     'store_duration_ms', checkpoint_artifacts.store_duration_ms
                 )
                 ORDER BY checkpoint_artifacts.role, checkpoint_artifacts.ordinal
             ) AS artifacts
        FROM checkpoint_artifacts
       WHERE checkpoint_artifacts.org_id = checkpoints.org_id
         AND checkpoint_artifacts.run_id = checkpoints.run_id
         AND checkpoint_artifacts.checkpoint_id = checkpoints.id
  ) AS checkpoint_artifacts ON true
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_execution_id = sqlc.arg(execution_id)
   AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
   AND runs.latest_checkpoint_id IS NOT NULL
   AND checkpoints.status = 'restoring'
   AND waitpoints.status = 'resolved'
   AND waitpoints.resolution_kind IS NOT NULL
 ORDER BY waitpoints.resolved_at DESC
 LIMIT 1;
