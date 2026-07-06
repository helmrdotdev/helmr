-- name: EnsureWorkspaceMountRequested :one
WITH locked_workspace AS MATERIALIZED (
    SELECT workspaces.*
      FROM workspaces
     WHERE workspaces.org_id = sqlc.arg(org_id)
       AND workspaces.worker_group_id = sqlc.arg(worker_group_id)
       AND workspaces.project_id = sqlc.arg(project_id)
       AND workspaces.environment_id = sqlc.arg(environment_id)
       AND workspaces.id = sqlc.arg(workspace_id)
       AND workspaces.state = 'active'
       AND workspaces.archived_at IS NULL
       AND workspaces.deleted_at IS NULL
     FOR UPDATE
),
existing_active_non_runnable AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM locked_workspace
      JOIN workspace_mounts
        ON workspace_mounts.org_id = locked_workspace.org_id
       AND workspace_mounts.worker_group_id = locked_workspace.worker_group_id
       AND workspace_mounts.project_id = locked_workspace.project_id
       AND workspace_mounts.environment_id = locked_workspace.environment_id
       AND workspace_mounts.workspace_id = locked_workspace.id
     WHERE workspace_mounts.state = 'unmounting'
),
inserted AS (
    INSERT INTO workspace_mounts (
        id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        workspace_id,
        deployment_sandbox_id,
        sandbox_fingerprint,
        base_version_id,
        image_artifact_id,
        image_artifact_format,
        rootfs_digest,
        image_digest,
        image_format,
        workspace_artifact_id,
        workspace_artifact_encoding,
        workspace_artifact_entry_count,
        workspace_artifact_digest,
        workspace_artifact_size_bytes,
        workspace_artifact_media_type,
        workspace_mount_path,
        runtime_abi,
        guestd_abi,
        adapter_abi,
        priority,
        state,
        request
    )
    SELECT sqlc.arg(id),
           workspaces.org_id,
           workspaces.worker_group_id,
           workspaces.project_id,
           workspaces.environment_id,
           workspaces.id,
           workspaces.deployment_sandbox_id,
           workspaces.sandbox_fingerprint,
           workspaces.current_version_id,
           deployment_sandboxes.image_artifact_id,
           deployment_sandboxes.image_artifact_format,
           deployment_sandboxes.rootfs_digest,
           deployment_sandboxes.image_digest,
           deployment_sandboxes.image_format,
           current_workspace_version.artifact_id,
           current_workspace_version.artifact_encoding,
           current_workspace_version.artifact_entry_count,
           workspace_artifact.digest,
           workspace_artifact.size_bytes,
           workspace_artifact.media_type,
           deployment_sandboxes.workspace_mount_path,
           deployment_sandboxes.runtime_abi,
           deployment_sandboxes.guestd_abi,
           deployment_sandboxes.adapter_abi,
           sqlc.arg(request_priority)::integer,
           'mounting',
           coalesce(sqlc.arg(request)::jsonb, '{}'::jsonb)
      FROM locked_workspace AS workspaces
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspaces.org_id
       AND deployment_sandboxes.project_id = workspaces.project_id
       AND deployment_sandboxes.environment_id = workspaces.environment_id
       AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = deployment_sandboxes.org_id
       AND image_artifact.project_id = deployment_sandboxes.project_id
       AND image_artifact.environment_id = deployment_sandboxes.environment_id
       AND image_artifact.id = deployment_sandboxes.image_artifact_id
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
      JOIN workspace_versions AS current_workspace_version
        ON current_workspace_version.org_id = workspaces.org_id
       AND current_workspace_version.project_id = workspaces.project_id
       AND current_workspace_version.environment_id = workspaces.environment_id
       AND current_workspace_version.workspace_id = workspaces.id
       AND current_workspace_version.id = workspaces.current_version_id
       AND current_workspace_version.state = 'ready'
      JOIN artifacts AS workspace_artifact
        ON workspace_artifact.org_id = current_workspace_version.org_id
       AND workspace_artifact.project_id = current_workspace_version.project_id
       AND workspace_artifact.environment_id = current_workspace_version.environment_id
       AND workspace_artifact.id = current_workspace_version.artifact_id
       AND workspace_artifact.kind = 'workspace_version'
       AND workspace_artifact.media_type = 'application/vnd.helmr.workspace.v0.tar'
     WHERE NOT EXISTS (SELECT 1 FROM existing_active_non_runnable)
    ON CONFLICT (workspace_id) WHERE state IN ('mounting', 'mounted', 'unmounting')
    DO UPDATE SET priority = GREATEST(workspace_mounts.priority, excluded.priority),
                  updated_at = CASE
                      WHEN workspace_mounts.priority < excluded.priority THEN now()
                      ELSE workspace_mounts.updated_at
                  END
    WHERE workspace_mounts.state IN ('mounting', 'mounted')
      AND workspace_mounts.worker_group_id = excluded.worker_group_id
    RETURNING workspace_mounts.*,
              (workspace_mounts.xmax = 0)::boolean AS inserted,
              CASE
                  WHEN workspace_mounts.xmax = 0 THEN 'created'
                  ELSE 'reused_conflict'
              END::text AS decision
),
reactivated_inserted_workspace AS (
    UPDATE workspaces
       SET desired_state = 'active',
           updated_at = now()
      FROM inserted
     WHERE workspaces.org_id = inserted.org_id
       AND workspaces.project_id = inserted.project_id
       AND workspaces.environment_id = inserted.environment_id
       AND workspaces.id = inserted.workspace_id
       AND workspaces.desired_state <> 'active'
    RETURNING workspaces.id
)
SELECT inserted.*
  FROM inserted
 LIMIT 1;

-- name: ClassifyRunWorkspaceReuse :one
WITH run_scope AS MATERIALIZED (
    SELECT runs.id AS run_id,
           runs.org_id,
           runs.project_id,
           runs.environment_id,
           runs.workspace_id,
           runs.workspace_mount_id AS queued_workspace_mount_id,
           run_runtime_requirements.worker_group_id AS required_worker_group_id,
           run_runtime_requirements.requested_milli_cpu,
           run_runtime_requirements.requested_memory_mib,
           run_runtime_requirements.requested_disk_mib,
           run_runtime_requirements.requested_execution_slots,
           run_runtime_requirements.runtime_id,
           run_runtime_requirements.runtime_arch,
           run_runtime_requirements.runtime_abi,
           run_runtime_requirements.kernel_digest,
           run_runtime_requirements.initramfs_digest,
           run_runtime_requirements.rootfs_digest,
           run_runtime_requirements.cni_profile
      FROM runs
      JOIN run_runtime_requirements
        ON run_runtime_requirements.org_id = runs.org_id
       AND run_runtime_requirements.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.workspace_id IS NOT NULL
),
worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
),
active_mounts AS MATERIALIZED (
    SELECT workspace_mounts.*,
           runtime_instances.worker_instance_id AS mounted_worker_instance_id,
           runtime_instances.runtime_release_id AS mounted_runtime_id
      FROM run_scope
      JOIN workspace_mounts
        ON workspace_mounts.org_id = run_scope.org_id
       AND workspace_mounts.project_id = run_scope.project_id
       AND workspace_mounts.environment_id = run_scope.environment_id
       AND workspace_mounts.workspace_id = run_scope.workspace_id
      LEFT JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
     WHERE workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
),
resident_mount AS MATERIALIZED (
    SELECT active_mounts.*
      FROM active_mounts
     ORDER BY CASE active_mounts.state
                  WHEN 'mounted' THEN 0
                  WHEN 'mounting' THEN 1
                  WHEN 'unmounting' THEN 2
                  ELSE 3
              END,
              active_mounts.created_at DESC
     LIMIT 1
),
active_write_leases AS MATERIALIZED (
    SELECT workspace_leases.id
      FROM run_scope
      JOIN workspace_leases
        ON workspace_leases.org_id = run_scope.org_id
       AND workspace_leases.project_id = run_scope.project_id
       AND workspace_leases.environment_id = run_scope.environment_id
       AND workspace_leases.workspace_id = run_scope.workspace_id
     WHERE workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
       AND workspace_leases.expires_at > now()
)
SELECT run_scope.run_id,
       run_scope.workspace_id,
       run_scope.queued_workspace_mount_id,
       resident_mount.id AS resident_workspace_mount_id,
       resident_mount.state AS resident_workspace_mount_state,
       resident_mount.mounted_worker_instance_id AS resident_worker_instance_id,
       sqlc.arg(worker_instance_id)::uuid AS dispatched_worker_instance_id,
       (SELECT count(*)::integer FROM active_mounts) AS active_mount_count,
       (SELECT count(*)::integer FROM active_mounts WHERE state = 'mounted') AS mounted_mount_count,
       (SELECT count(*)::integer FROM active_mounts WHERE state = 'mounting') AS mounting_mount_count,
       (SELECT count(*)::integer FROM active_write_leases) AS active_write_lease_count,
       EXISTS (
           SELECT 1
             FROM worker_scope
            WHERE worker_scope.worker_group_id = run_scope.required_worker_group_id
       ) AS worker_group_matches,
       EXISTS (
           SELECT 1
             FROM worker_scope
            WHERE worker_scope.runtime_id = run_scope.runtime_id
              AND worker_scope.runtime_arch = run_scope.runtime_arch
              AND worker_scope.runtime_abi = run_scope.runtime_abi
              AND worker_scope.kernel_digest = run_scope.kernel_digest
              AND worker_scope.initramfs_digest = run_scope.initramfs_digest
              AND worker_scope.rootfs_digest = run_scope.rootfs_digest
              AND worker_scope.cni_profile = run_scope.cni_profile
       ) AS worker_runtime_matches,
       EXISTS (
           SELECT 1
             FROM worker_scope
            WHERE worker_scope.available_milli_cpu >= run_scope.requested_milli_cpu
              AND worker_scope.available_memory_mib >= run_scope.requested_memory_mib
              AND worker_scope.available_disk_mib >= run_scope.requested_disk_mib
              AND worker_scope.available_execution_slots >= run_scope.requested_execution_slots
       ) AS worker_capacity_fits,
       CASE
           WHEN EXISTS (SELECT 1 FROM active_write_leases) THEN 'active_write_lease'
           WHEN EXISTS (
               SELECT 1
                 FROM active_mounts
                WHERE active_mounts.state = 'mounted'
                  AND active_mounts.mounted_worker_instance_id = sqlc.arg(worker_instance_id)
           ) THEN 'same_worker_mounted'
           WHEN EXISTS (
               SELECT 1
                 FROM active_mounts
                WHERE active_mounts.state = 'mounted'
                  AND active_mounts.mounted_worker_instance_id <> sqlc.arg(worker_instance_id)
           ) THEN 'mounted_on_different_worker'
           WHEN EXISTS (
               SELECT 1
                 FROM active_mounts
                WHERE active_mounts.state = 'mounting'
           ) THEN 'compatible_but_mounting'
           WHEN EXISTS (
               SELECT 1
                 FROM active_mounts
                WHERE active_mounts.state = 'unmounting'
           ) THEN 'active_non_runnable'
           WHEN NOT EXISTS (
               SELECT 1
                 FROM worker_scope
                WHERE worker_scope.worker_group_id = run_scope.required_worker_group_id
                  AND worker_scope.runtime_id = run_scope.runtime_id
                  AND worker_scope.runtime_arch = run_scope.runtime_arch
                  AND worker_scope.runtime_abi = run_scope.runtime_abi
                  AND worker_scope.kernel_digest = run_scope.kernel_digest
                  AND worker_scope.initramfs_digest = run_scope.initramfs_digest
                  AND worker_scope.rootfs_digest = run_scope.rootfs_digest
                  AND worker_scope.cni_profile = run_scope.cni_profile
                  AND worker_scope.available_milli_cpu >= run_scope.requested_milli_cpu
                  AND worker_scope.available_memory_mib >= run_scope.requested_memory_mib
                  AND worker_scope.available_disk_mib >= run_scope.requested_disk_mib
                  AND worker_scope.available_execution_slots >= run_scope.requested_execution_slots
           ) THEN 'worker_incompatible'
           ELSE 'no_active_mount'
       END::text AS outcome
  FROM run_scope
  LEFT JOIN resident_mount ON true;

-- name: GetWorkspaceMount :one
SELECT *
  FROM workspace_mounts
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: GetWorkspaceMountForWorkerPrimitiveScope :one
SELECT workspace_mounts.*
  FROM workspace_mounts
  JOIN runtime_instances
    ON runtime_instances.org_id = workspace_mounts.org_id
   AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
   AND runtime_instances.id = workspace_mounts.runtime_instance_id
  JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
    ON project_worker_group_placement.org_id = workspace_mounts.org_id
   AND project_worker_group_placement.project_id = workspace_mounts.project_id
   AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
   AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
   AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
  JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
            AND worker_groups.state = 'active'
 WHERE workspace_mounts.org_id = sqlc.arg(org_id)
   AND workspace_mounts.worker_group_id = sqlc.arg(worker_group_id)
   AND workspace_mounts.project_id = sqlc.arg(project_id)
   AND workspace_mounts.environment_id = sqlc.arg(environment_id)
   AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
   AND workspace_mounts.id = sqlc.arg(id)
   AND workspace_mounts.state IN ('mounted', 'unmounting')
   AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
   AND runtime_instances.workspace_mount_id = workspace_mounts.id
   AND runtime_instances.state IN ('running', 'waiting_hot', 'checkpointing', 'stopping')
   AND (
       runtime_instances.expires_at IS NULL
       OR runtime_instances.expires_at > now()
   );

-- name: GetWorkspaceMountPrerequisites :one
SELECT workspaces.id AS workspace_id,
       workspaces.current_version_id,
       current_workspace_version.id AS current_workspace_version_id,
       current_workspace_version.state AS current_workspace_version_state,
       current_workspace_version.artifact_id AS current_workspace_artifact_id,
       workspace_artifact.id AS workspace_artifact_id,
       workspace_artifact.digest AS workspace_artifact_digest,
       workspace_artifact.size_bytes AS workspace_artifact_size_bytes,
       workspace_artifact.media_type AS workspace_artifact_media_type,
       deployment_sandboxes.id AS deployment_sandbox_id,
       deployment_sandboxes.image_artifact_id AS sandbox_image_artifact_id,
       image_artifact.id AS image_artifact_id,
       image_artifact.digest AS image_artifact_digest,
       image_artifact.size_bytes AS image_artifact_size_bytes,
       image_artifact.media_type AS image_artifact_media_type,
       active_mount.state AS active_mount_state
  FROM workspaces
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = workspaces.org_id
   AND deployment_sandboxes.project_id = workspaces.project_id
   AND deployment_sandboxes.environment_id = workspaces.environment_id
   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
  LEFT JOIN artifacts AS image_artifact
    ON image_artifact.org_id = deployment_sandboxes.org_id
   AND image_artifact.project_id = deployment_sandboxes.project_id
   AND image_artifact.environment_id = deployment_sandboxes.environment_id
   AND image_artifact.id = deployment_sandboxes.image_artifact_id
  LEFT JOIN workspace_versions AS current_workspace_version
    ON current_workspace_version.org_id = workspaces.org_id
   AND current_workspace_version.project_id = workspaces.project_id
   AND current_workspace_version.environment_id = workspaces.environment_id
   AND current_workspace_version.workspace_id = workspaces.id
   AND current_workspace_version.id = workspaces.current_version_id
  LEFT JOIN artifacts AS workspace_artifact
    ON workspace_artifact.org_id = current_workspace_version.org_id
   AND workspace_artifact.project_id = current_workspace_version.project_id
   AND workspace_artifact.environment_id = current_workspace_version.environment_id
   AND workspace_artifact.id = current_workspace_version.artifact_id
  LEFT JOIN workspace_mounts AS active_mount
    ON active_mount.org_id = workspaces.org_id
   AND active_mount.worker_group_id = workspaces.worker_group_id
   AND active_mount.project_id = workspaces.project_id
   AND active_mount.environment_id = workspaces.environment_id
   AND active_mount.workspace_id = workspaces.id
   AND active_mount.state IN ('mounting', 'mounted', 'unmounting')
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.state = 'active'
   AND workspaces.archived_at IS NULL
   AND workspaces.deleted_at IS NULL;

-- name: ClaimWorkspaceMount :one
WITH worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
active_run_usage AS MATERIALIZED (
    SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
      FROM worker_scope
      JOIN run_leases ON run_leases.worker_instance_id = worker_scope.id
                     AND run_leases.status IN ('leased', 'running')
      JOIN runs ON runs.org_id = run_leases.org_id
               AND runs.id = run_leases.run_id
               AND runs.workspace_mount_id IS NULL
      JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                                   AND run_runtime_requirements.run_id = run_leases.run_id
),
active_runtime_instance_usage AS MATERIALIZED (
    SELECT COALESCE(sum(runtime_instances.reserved_cpu_millis), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(runtime_instances.reserved_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(runtime_instances.reserved_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(runtime_instances.reserved_execution_slots), 0)::int AS used_slots
      FROM worker_scope
      JOIN runtime_instances
        ON runtime_instances.worker_instance_id = worker_scope.id
       AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
),
candidate AS (
    SELECT workspace_mounts.id,
           workspace_mounts.org_id,
           workspace_mounts.worker_group_id,
           workspace_mounts.project_id,
           workspace_mounts.environment_id,
           workspace_mounts.workspace_id,
           workspace_mounts.deployment_sandbox_id,
           workspace_mounts.sandbox_fingerprint,
           workspace_mounts.base_version_id,
           workspace_mounts.image_artifact_id,
           workspace_mounts.image_artifact_format,
           workspace_mounts.rootfs_digest,
           workspace_mounts.image_digest,
           workspace_mounts.image_format,
           workspace_mounts.workspace_mount_path,
           workspace_mounts.runtime_abi,
           workspace_mounts.guestd_abi,
           workspace_mounts.adapter_abi,
           image_artifact.digest AS image_artifact_digest,
           ready_runtime_instance.id AS runtime_instance_id,
           ready_runtime_instance.instance_token AS runtime_instance_token,
           coalesce((deployment_sandboxes.resource_floor->>'milli_cpu')::integer, 1000) AS sandbox_floor_cpu_millis,
           coalesce((deployment_sandboxes.resource_floor->>'memory_mib')::integer, 1024) AS sandbox_floor_memory_mib,
           deployment_sandboxes.disk_floor_mib AS sandbox_floor_disk_mib,
	           1::integer AS sandbox_floor_execution_slots
	      FROM workspace_mounts
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspace_mounts.org_id
       AND deployment_sandboxes.project_id = workspace_mounts.project_id
       AND deployment_sandboxes.environment_id = workspace_mounts.environment_id
       AND deployment_sandboxes.id = workspace_mounts.deployment_sandbox_id
       AND deployment_sandboxes.fingerprint = workspace_mounts.sandbox_fingerprint
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
      JOIN worker_scope ON worker_scope.worker_group_id = workspace_mounts.worker_group_id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
        ON project_worker_group_placement.org_id = workspace_mounts.org_id
	       AND project_worker_group_placement.project_id = workspace_mounts.project_id
	       AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
	       AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
	       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
	      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
	                AND worker_groups.state = 'active'
	      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = workspace_mounts.org_id
       AND image_artifact.project_id = workspace_mounts.project_id
       AND image_artifact.environment_id = workspace_mounts.environment_id
       AND image_artifact.id = workspace_mounts.image_artifact_id
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
	      CROSS JOIN active_run_usage
	      CROSS JOIN active_runtime_instance_usage
	      LEFT JOIN LATERAL (
          SELECT runtime_instances.*
            FROM runtime_instances
           WHERE runtime_instances.worker_instance_id = worker_scope.id
             AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
             AND runtime_instances.state = 'ready'
             AND (
                 runtime_instances.expires_at IS NULL
                 OR runtime_instances.expires_at > now()
             )
             AND runtime_instances.runtime_release_id = sqlc.arg(runtime_id)
             AND runtime_instances.deployment_sandbox_id = workspace_mounts.deployment_sandbox_id
             AND runtime_instances.sandbox_fingerprint = workspace_mounts.sandbox_fingerprint
             AND runtime_instances.rootfs_digest = workspace_mounts.rootfs_digest
             AND runtime_instances.image_digest = workspace_mounts.image_digest
             AND runtime_instances.image_format = workspace_mounts.image_format
             AND runtime_instances.sandbox_image_artifact_id = workspace_mounts.image_artifact_id
             AND runtime_instances.sandbox_image_artifact_digest = image_artifact.digest
             AND runtime_instances.sandbox_image_artifact_format = workspace_mounts.image_artifact_format
             AND runtime_instances.workspace_mount_path = workspace_mounts.workspace_mount_path
             AND runtime_instances.runtime_abi = workspace_mounts.runtime_abi
             AND runtime_instances.guestd_abi = workspace_mounts.guestd_abi
             AND runtime_instances.adapter_abi = workspace_mounts.adapter_abi
             AND (
                 runtime_instances.adopting_workspace_mount_id IS NULL
                 OR runtime_instances.adopting_workspace_mount_id = workspace_mounts.id
             )
           ORDER BY runtime_instances.prepared_at ASC NULLS LAST,
                    runtime_instances.created_at ASC
           LIMIT 1
           FOR UPDATE SKIP LOCKED
      ) ready_runtime_instance ON true
      LEFT JOIN LATERAL (
          SELECT runtime_instances.id
            FROM runtime_instances
            JOIN worker_instances AS preparing_worker
              ON preparing_worker.id = runtime_instances.worker_instance_id
             AND preparing_worker.worker_group_id = workspace_mounts.worker_group_id
             AND preparing_worker.status = 'active'
             AND preparing_worker.worker_group_id = worker_scope.worker_group_id
           WHERE runtime_instances.state = 'preparing'
             AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
             AND runtime_instances.workspace_mount_id IS NULL
             AND (
                 runtime_instances.expires_at IS NULL
                 OR runtime_instances.expires_at > now()
             )
             AND runtime_instances.runtime_release_id = sqlc.arg(runtime_id)
             AND runtime_instances.deployment_sandbox_id = workspace_mounts.deployment_sandbox_id
             AND runtime_instances.sandbox_fingerprint = workspace_mounts.sandbox_fingerprint
             AND runtime_instances.rootfs_digest = workspace_mounts.rootfs_digest
             AND runtime_instances.image_digest = workspace_mounts.image_digest
             AND runtime_instances.image_format = workspace_mounts.image_format
             AND runtime_instances.sandbox_image_artifact_id = workspace_mounts.image_artifact_id
             AND runtime_instances.sandbox_image_artifact_digest = image_artifact.digest
             AND runtime_instances.sandbox_image_artifact_format = workspace_mounts.image_artifact_format
             AND runtime_instances.workspace_mount_path = workspace_mounts.workspace_mount_path
             AND runtime_instances.runtime_abi = workspace_mounts.runtime_abi
             AND runtime_instances.guestd_abi = workspace_mounts.guestd_abi
             AND runtime_instances.adapter_abi = workspace_mounts.adapter_abi
           ORDER BY runtime_instances.created_at ASC
           LIMIT 1
      ) preparing_runtime_instance ON true
     WHERE workspace_mounts.state = 'mounting'
       AND workspace_mounts.runtime_instance_id IS NULL
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.org_id = workspace_mounts.org_id
              AND workspace_leases.workspace_id = workspace_mounts.workspace_id
              AND workspace_leases.workspace_mount_id = workspace_mounts.id
              AND workspace_leases.lease_kind = 'write'
              AND workspace_leases.state IN ('active', 'releasing')
              AND workspace_leases.expires_at > now()
       )
       AND workspace_mounts.rootfs_digest = sqlc.arg(rootfs_digest)
       AND workspace_mounts.runtime_abi = sqlc.arg(runtime_abi)
       AND workspace_mounts.guestd_abi = sqlc.arg(guestd_abi)
       AND workspace_mounts.adapter_abi = sqlc.arg(adapter_abi)
       AND (
           ready_runtime_instance.id IS NOT NULL
           OR (
               preparing_runtime_instance.id IS NULL
               AND coalesce((deployment_sandboxes.resource_floor->>'milli_cpu')::integer, 1000) <= GREATEST(worker_scope.available_milli_cpu - active_run_usage.used_milli_cpu - active_runtime_instance_usage.used_milli_cpu, 0)
               AND coalesce((deployment_sandboxes.resource_floor->>'memory_mib')::integer, 1024) <= GREATEST(worker_scope.available_memory_mib - active_run_usage.used_memory_mib - active_runtime_instance_usage.used_memory_mib, 0)
               AND deployment_sandboxes.disk_floor_mib <= GREATEST(worker_scope.available_disk_mib - active_run_usage.used_disk_mib - active_runtime_instance_usage.used_disk_mib, 0)
               AND 1 <= GREATEST(worker_scope.available_execution_slots - active_run_usage.used_slots - active_runtime_instance_usage.used_slots, 0)
           )
       )
     ORDER BY workspace_mounts.priority DESC,
              workspace_mounts.created_at ASC,
              workspace_mounts.id ASC
	     LIMIT 1
	     FOR UPDATE OF workspace_mounts SKIP LOCKED
),
claimed_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'binding',
           runtime_epoch = runtime_instances.runtime_epoch + 1,
           workspace_mount_id = candidate.id,
           owner_workspace_id = candidate.workspace_id,
           owner_workspace_version_id = candidate.base_version_id,
           adopting_workspace_mount_id = NULL,
           adoption_expires_at = NULL,
           bound_at = now(),
           expires_at = NULL,
           updated_at = now()
      FROM candidate
     WHERE runtime_instances.org_id = candidate.org_id
       AND runtime_instances.id = candidate.runtime_instance_id
       AND runtime_instances.state = 'ready'
	     RETURNING runtime_instances.id,
	              runtime_instances.instance_token,
	              runtime_instances.runtime_epoch,
	              runtime_instances.reserved_cpu_millis,
	              runtime_instances.reserved_memory_mib,
	              runtime_instances.reserved_disk_mib,
	              runtime_instances.reserved_execution_slots
),
cold_runtime_key AS MATERIALIZED (
    SELECT jsonb_build_object(
               'runtime_id', sqlc.arg(runtime_id)::text,
               'deployment_sandbox_id', candidate.deployment_sandbox_id::text,
               'image_digest', candidate.image_digest,
               'image_format', candidate.image_format,
               'rootfs_digest', candidate.rootfs_digest,
               'runtime_abi', candidate.runtime_abi,
               'guestd_abi', candidate.guestd_abi,
               'adapter_abi', candidate.adapter_abi,
               'workspace_mount_path', candidate.workspace_mount_path,
               'sandbox_artifact_digest', candidate.image_artifact_digest,
               'sandbox_artifact_format', candidate.image_artifact_format,
               'network', COALESCE(sqlc.arg(network_policy)::jsonb, '{}'::jsonb)
           ) AS value
      FROM candidate
     WHERE candidate.runtime_instance_id IS NULL
),
cold_runtime_instance AS (
    INSERT INTO runtime_instances (
        id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        worker_instance_id,
        runtime_release_id,
        deployment_sandbox_id,
        runtime_key_hash,
        runtime_key,
        sandbox_fingerprint,
        rootfs_digest,
        image_digest,
        image_format,
        sandbox_image_artifact_id,
        sandbox_image_artifact_digest,
        sandbox_image_artifact_format,
        workspace_mount_path,
        runtime_abi,
        guestd_abi,
        adapter_abi,
        network_policy,
        reserved_cpu_millis,
        reserved_memory_mib,
        reserved_disk_mib,
        reserved_execution_slots,
        workspace_mount_id,
        owner_workspace_id,
        owner_workspace_version_id,
        state,
        instance_token,
        last_heartbeat_at,
        bound_at
    )
    SELECT sqlc.arg(runtime_instance_id),
           candidate.org_id,
           candidate.worker_group_id,
           candidate.project_id,
           candidate.environment_id,
           sqlc.arg(worker_instance_id),
           sqlc.arg(runtime_id),
           candidate.deployment_sandbox_id,
           md5(cold_runtime_key.value::text),
           cold_runtime_key.value,
           candidate.sandbox_fingerprint,
           candidate.rootfs_digest,
           candidate.image_digest,
           candidate.image_format,
           candidate.image_artifact_id,
           candidate.image_artifact_digest,
           candidate.image_artifact_format,
           candidate.workspace_mount_path,
           candidate.runtime_abi,
           candidate.guestd_abi,
           candidate.adapter_abi,
           COALESCE(sqlc.arg(network_policy)::jsonb, '{}'::jsonb),
           candidate.sandbox_floor_cpu_millis,
           candidate.sandbox_floor_memory_mib,
           candidate.sandbox_floor_disk_mib,
           candidate.sandbox_floor_execution_slots,
           candidate.id,
           candidate.workspace_id,
           candidate.base_version_id,
           'binding',
           sqlc.arg(runtime_instance_token),
           now(),
           now()
      FROM candidate, cold_runtime_key
     WHERE candidate.runtime_instance_id IS NULL
    RETURNING runtime_instances.id,
              runtime_instances.instance_token,
              runtime_instances.runtime_epoch,
              runtime_instances.reserved_cpu_millis,
              runtime_instances.reserved_memory_mib,
              runtime_instances.reserved_disk_mib,
              runtime_instances.reserved_execution_slots
),
runtime_instance_capacity AS (
    SELECT *
      FROM claimed_runtime_instance
    UNION ALL
    SELECT *
      FROM cold_runtime_instance
),
claimed AS (
    UPDATE workspace_mounts
       SET runtime_instance_id = runtime_instance_capacity.id,
           guestd_channel_token_hash = sqlc.arg(guestd_channel_token_hash),
           guestd_channel_token_expires_at = sqlc.arg(guestd_channel_token_expires_at),
           last_heartbeat_at = now(),
           updated_at = now()
      FROM candidate
      JOIN runtime_instance_capacity ON true
     WHERE workspace_mounts.org_id = candidate.org_id
       AND workspace_mounts.id = candidate.id
    RETURNING workspace_mounts.*
)
SELECT claimed.*,
       image_artifact.digest AS image_artifact_digest,
       image_artifact.size_bytes AS image_artifact_size_bytes,
       image_artifact.media_type AS image_artifact_media_type,
       sqlc.arg(runtime_id)::text AS runtime_id,
       runtime_instance_capacity.instance_token AS runtime_instance_token,
       runtime_instance_capacity.runtime_epoch AS runtime_epoch,
       runtime_instance_capacity.reserved_cpu_millis AS requested_cpu_millis,
       runtime_instance_capacity.reserved_memory_mib AS requested_memory_mib,
       runtime_instance_capacity.reserved_disk_mib AS requested_disk_mib,
       runtime_instance_capacity.reserved_execution_slots AS requested_execution_slots
  FROM claimed
  JOIN runtime_instance_capacity ON runtime_instance_capacity.id = claimed.runtime_instance_id
  JOIN artifacts AS image_artifact
    ON image_artifact.org_id = claimed.org_id
   AND image_artifact.project_id = claimed.project_id
   AND image_artifact.environment_id = claimed.environment_id
   AND image_artifact.id = claimed.image_artifact_id
   AND image_artifact.kind = 'sandbox_image'
   AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
;

-- name: ReserveWorkspaceMountPreparingRuntime :one
WITH worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
candidate AS (
    SELECT workspace_mounts.id,
           workspace_mounts.org_id,
           preparing_runtime_instance.id AS preparing_runtime_instance_id
      FROM workspace_mounts
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspace_mounts.org_id
       AND deployment_sandboxes.project_id = workspace_mounts.project_id
       AND deployment_sandboxes.environment_id = workspace_mounts.environment_id
       AND deployment_sandboxes.id = workspace_mounts.deployment_sandbox_id
       AND deployment_sandboxes.fingerprint = workspace_mounts.sandbox_fingerprint
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
      JOIN worker_scope ON worker_scope.worker_group_id = workspace_mounts.worker_group_id
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = workspace_mounts.org_id
       AND image_artifact.project_id = workspace_mounts.project_id
       AND image_artifact.environment_id = workspace_mounts.environment_id
       AND image_artifact.id = workspace_mounts.image_artifact_id
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
      JOIN LATERAL (
          SELECT runtime_instances.*
            FROM runtime_instances
           WHERE runtime_instances.worker_instance_id = worker_scope.id
             AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
             AND runtime_instances.state = 'preparing'
             AND runtime_instances.workspace_mount_id IS NULL
             AND runtime_instances.adopting_workspace_mount_id IS NULL
             AND (
                 runtime_instances.expires_at IS NULL
                 OR runtime_instances.expires_at > now()
             )
             AND runtime_instances.runtime_release_id = sqlc.arg(runtime_id)
             AND runtime_instances.deployment_sandbox_id = workspace_mounts.deployment_sandbox_id
             AND runtime_instances.sandbox_fingerprint = workspace_mounts.sandbox_fingerprint
             AND runtime_instances.rootfs_digest = workspace_mounts.rootfs_digest
             AND runtime_instances.image_digest = workspace_mounts.image_digest
             AND runtime_instances.image_format = workspace_mounts.image_format
             AND runtime_instances.sandbox_image_artifact_id = workspace_mounts.image_artifact_id
             AND runtime_instances.sandbox_image_artifact_digest = image_artifact.digest
             AND runtime_instances.sandbox_image_artifact_format = workspace_mounts.image_artifact_format
             AND runtime_instances.workspace_mount_path = workspace_mounts.workspace_mount_path
             AND runtime_instances.runtime_abi = workspace_mounts.runtime_abi
             AND runtime_instances.guestd_abi = workspace_mounts.guestd_abi
             AND runtime_instances.adapter_abi = workspace_mounts.adapter_abi
           ORDER BY runtime_instances.created_at ASC
           LIMIT 1
           FOR UPDATE SKIP LOCKED
      ) preparing_runtime_instance ON true
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
        ON project_worker_group_placement.org_id = workspace_mounts.org_id
       AND project_worker_group_placement.project_id = workspace_mounts.project_id
       AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
       AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
       AND project_worker_group_placement.worker_group_id = worker_scope.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE workspace_mounts.state = 'mounting'
       AND workspace_mounts.runtime_instance_id IS NULL
       AND workspace_mounts.rootfs_digest = sqlc.arg(rootfs_digest)
       AND workspace_mounts.runtime_abi = sqlc.arg(runtime_abi)
       AND workspace_mounts.guestd_abi = sqlc.arg(guestd_abi)
       AND workspace_mounts.adapter_abi = sqlc.arg(adapter_abi)
     ORDER BY workspace_mounts.priority DESC,
              workspace_mounts.created_at ASC,
              workspace_mounts.id ASC
     LIMIT 1
     FOR UPDATE OF workspace_mounts SKIP LOCKED
),
reserved_runtime_instance AS (
    UPDATE runtime_instances
       SET adopting_workspace_mount_id = candidate.id,
           adoption_expires_at = sqlc.arg(guestd_channel_token_expires_at),
           updated_at = now()
      FROM candidate
     WHERE runtime_instances.id = candidate.preparing_runtime_instance_id
       AND runtime_instances.state = 'preparing'
       AND runtime_instances.workspace_mount_id IS NULL
       AND runtime_instances.adopting_workspace_mount_id IS NULL
    RETURNING runtime_instances.id
),
reserved_mount AS (
    UPDATE workspace_mounts
       SET updated_at = now()
      FROM candidate
     WHERE workspace_mounts.org_id = candidate.org_id
       AND workspace_mounts.id = candidate.id
       AND EXISTS (SELECT 1 FROM reserved_runtime_instance)
    RETURNING workspace_mounts.*
)
SELECT reserved_mount.*,
       candidate.preparing_runtime_instance_id
  FROM reserved_mount
  JOIN candidate ON candidate.org_id = reserved_mount.org_id
                AND candidate.id = reserved_mount.id;

-- name: GetAwaitingPreparedRuntimeMountForWorker :one
SELECT workspace_mounts.id,
       runtime_instances.id AS preparing_runtime_instance_id
  FROM workspace_mounts
  JOIN runtime_instances
    ON runtime_instances.org_id = workspace_mounts.org_id
   AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
   AND runtime_instances.adopting_workspace_mount_id = workspace_mounts.id
   AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND runtime_instances.state = 'preparing'
   AND (
       runtime_instances.expires_at IS NULL
       OR runtime_instances.expires_at > now()
   )
  JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                       AND worker_instances.worker_group_id = runtime_instances.worker_group_id
                       AND worker_instances.status = 'active'
  JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
    ON project_worker_group_placement.org_id = workspace_mounts.org_id
   AND project_worker_group_placement.project_id = workspace_mounts.project_id
   AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
   AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
   AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
  JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
            AND worker_groups.state = 'active'
 WHERE workspace_mounts.state = 'mounting'
   AND workspace_mounts.runtime_instance_id IS NULL
   AND workspace_mounts.rootfs_digest = sqlc.arg(rootfs_digest)
   AND workspace_mounts.runtime_abi = sqlc.arg(runtime_abi)
   AND workspace_mounts.guestd_abi = sqlc.arg(guestd_abi)
   AND workspace_mounts.adapter_abi = sqlc.arg(adapter_abi)
   AND runtime_instances.runtime_release_id = sqlc.arg(runtime_id)
 ORDER BY workspace_mounts.priority DESC,
          workspace_mounts.created_at ASC,
          workspace_mounts.id ASC
 LIMIT 1;

-- name: ReleaseExpiredPreparedRuntimeReservations :many
WITH target AS MATERIALIZED (
    SELECT runtime_instances.id,
           runtime_instances.org_id,
           runtime_instances.adopting_workspace_mount_id AS workspace_mount_id,
           runtime_instances.state
      FROM runtime_instances
     WHERE runtime_instances.adopting_workspace_mount_id IS NOT NULL
       AND runtime_instances.adoption_expires_at IS NOT NULL
       AND runtime_instances.adoption_expires_at <= sqlc.arg(expired_before)
       AND runtime_instances.state IN ('preparing', 'ready')
     FOR UPDATE OF runtime_instances SKIP LOCKED
),
expired_runtime_instances AS (
    UPDATE runtime_instances
       SET adopting_workspace_mount_id = NULL,
           adoption_expires_at = NULL,
           expires_at = CASE
               WHEN target.state = 'preparing' THEN LEAST(COALESCE(runtime_instances.expires_at, sqlc.arg(expired_before)), sqlc.arg(expired_before))
               ELSE runtime_instances.expires_at
           END,
           last_reclaim_reason = CASE
               WHEN target.state = 'preparing' THEN 'adoption_expired'
               ELSE runtime_instances.last_reclaim_reason
           END,
           updated_at = now()
      FROM target
     WHERE runtime_instances.id = target.id
    RETURNING target.org_id,
              target.workspace_mount_id
),
released_mounts AS (
    UPDATE workspace_mounts
       SET updated_at = now()
      FROM expired_runtime_instances
     WHERE workspace_mounts.org_id = expired_runtime_instances.org_id
       AND workspace_mounts.id = expired_runtime_instances.workspace_mount_id
       AND workspace_mounts.state = 'mounting'
    RETURNING workspace_mounts.*
)
SELECT *
  FROM released_mounts;

-- name: RenewWorkspaceMount :one
WITH renewed_runtime_instance AS (
    UPDATE runtime_instances
       SET last_heartbeat_at = now(),
           updated_at = now()
      FROM worker_instances
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement ON true
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE runtime_instances.org_id = sqlc.arg(org_id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_instances.id = runtime_instances.worker_instance_id
       AND worker_instances.worker_group_id = runtime_instances.worker_group_id
       AND project_worker_group_placement.org_id = runtime_instances.org_id
       AND project_worker_group_placement.project_id = runtime_instances.project_id
       AND project_worker_group_placement.environment_id = runtime_instances.environment_id
       AND project_worker_group_placement.worker_group_id = runtime_instances.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
       AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
       AND runtime_instances.state IN ('binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
    RETURNING runtime_instances.*
),
renewed_mount AS (
    UPDATE workspace_mounts
       SET guestd_channel_token_expires_at = sqlc.arg(guestd_channel_token_expires_at),
           last_heartbeat_at = now(),
           updated_at = now()
      FROM renewed_runtime_instance
     WHERE workspace_mounts.org_id = renewed_runtime_instance.org_id
       AND workspace_mounts.worker_group_id = renewed_runtime_instance.worker_group_id
       AND workspace_mounts.id = sqlc.arg(id)
       AND workspace_mounts.runtime_instance_id = renewed_runtime_instance.id
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.*
)
SELECT *
  FROM renewed_mount;

-- name: MarkWorkspaceMountMounted :one
WITH authenticated_runtime_instance AS MATERIALIZED (
    SELECT runtime_instances.*
      FROM runtime_instances
      JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                           AND worker_instances.worker_group_id = runtime_instances.worker_group_id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement ON project_worker_group_placement.org_id = runtime_instances.org_id
                            AND project_worker_group_placement.project_id = runtime_instances.project_id
                            AND project_worker_group_placement.environment_id = runtime_instances.environment_id
                            AND project_worker_group_placement.worker_group_id = runtime_instances.worker_group_id
                            AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE runtime_instances.org_id = sqlc.arg(org_id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
       AND runtime_instances.state IN ('binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
     FOR UPDATE OF runtime_instances
),
updated_mount AS (
    UPDATE workspace_mounts
       SET state = CASE
               WHEN workspace_mounts.state = 'unmounting' THEN workspace_mounts.state
               ELSE 'mounted'::workspace_mount_state
           END,
           mounted_at = coalesce(mounted_at, now()),
           guestd_channel_token_expires_at = sqlc.arg(guestd_channel_token_expires_at),
           last_heartbeat_at = now(),
           updated_at = now()
      FROM authenticated_runtime_instance
     WHERE workspace_mounts.org_id = authenticated_runtime_instance.org_id
       AND workspace_mounts.worker_group_id = authenticated_runtime_instance.worker_group_id
       AND workspace_mounts.id = sqlc.arg(id)
       AND workspace_mounts.runtime_instance_id = authenticated_runtime_instance.id
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.*
),
updated_runtime_instance AS (
    UPDATE runtime_instances
       SET state = CASE
               WHEN updated_mount.state = 'mounted' THEN 'running'::runtime_instance_state
               WHEN updated_mount.state = 'unmounting' THEN 'stopping'::runtime_instance_state
               ELSE runtime_instances.state
           END,
           running_at = CASE
               WHEN updated_mount.state = 'mounted' THEN coalesce(runtime_instances.running_at, now())
               ELSE runtime_instances.running_at
           END,
           last_heartbeat_at = now(),
           owner_workspace_id = updated_mount.workspace_id,
           owner_workspace_version_id = updated_mount.base_version_id,
           updated_at = now()
      FROM updated_mount, authenticated_runtime_instance
     WHERE runtime_instances.org_id = authenticated_runtime_instance.org_id
       AND runtime_instances.id = authenticated_runtime_instance.id
       AND runtime_instances.id = updated_mount.runtime_instance_id
    RETURNING runtime_instances.id
)
SELECT *
  FROM updated_mount
 WHERE EXISTS (SELECT 1 FROM updated_runtime_instance);

-- name: RequestWorkspaceMountStop :one
WITH locked_workspace AS MATERIALIZED (
    SELECT workspaces.*
      FROM workspaces
     WHERE workspaces.org_id = sqlc.arg(org_id)
       AND workspaces.worker_group_id = sqlc.arg(worker_group_id)
       AND workspaces.project_id = sqlc.arg(project_id)
       AND workspaces.environment_id = sqlc.arg(environment_id)
       AND workspaces.id = sqlc.arg(workspace_id)
       AND workspaces.state = 'active'
       AND workspaces.archived_at IS NULL
       AND workspaces.deleted_at IS NULL
     FOR UPDATE
),
target AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM locked_workspace
      JOIN workspace_mounts
        ON workspace_mounts.org_id = locked_workspace.org_id
       AND workspace_mounts.worker_group_id = locked_workspace.worker_group_id
       AND workspace_mounts.project_id = locked_workspace.project_id
       AND workspace_mounts.environment_id = locked_workspace.environment_id
       AND workspace_mounts.workspace_id = locked_workspace.id
     WHERE workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
     ORDER BY workspace_mounts.created_at DESC
     LIMIT 1
     FOR UPDATE OF workspace_mounts
),
requested_without_runtime AS (
    UPDATE workspace_mounts
       SET state = 'unmounted',
           unmounted_at = coalesce(workspace_mounts.unmounted_at, now()),
           stopped_at = coalesce(workspace_mounts.stopped_at, now()),
           updated_at = now()
      FROM target
     WHERE workspace_mounts.org_id = target.org_id
       AND workspace_mounts.worker_group_id = target.worker_group_id
       AND workspace_mounts.id = target.id
       AND target.runtime_instance_id IS NULL
    RETURNING workspace_mounts.*
),
requested_live_stop AS (
    UPDATE workspace_mounts
       SET state = 'unmounting',
           updated_at = now()
      FROM target
     WHERE workspace_mounts.org_id = target.org_id
       AND workspace_mounts.worker_group_id = target.worker_group_id
       AND workspace_mounts.id = target.id
       AND target.runtime_instance_id IS NOT NULL
    RETURNING workspace_mounts.*
),
released_prepared_runtime_reservation AS (
    UPDATE runtime_instances
       SET adopting_workspace_mount_id = NULL,
           adoption_expires_at = NULL,
           updated_at = now()
      FROM requested_without_runtime
     WHERE runtime_instances.org_id = requested_without_runtime.org_id
       AND runtime_instances.worker_group_id = requested_without_runtime.worker_group_id
       AND runtime_instances.adopting_workspace_mount_id = requested_without_runtime.id
       AND runtime_instances.state IN ('preparing', 'ready')
    RETURNING runtime_instances.id
),
updated_workspace AS (
    UPDATE workspaces
       SET desired_state = 'stopped',
           updated_at = now()
      FROM locked_workspace
     WHERE workspaces.org_id = locked_workspace.org_id
       AND workspaces.worker_group_id = locked_workspace.worker_group_id
       AND workspaces.project_id = locked_workspace.project_id
       AND workspaces.environment_id = locked_workspace.environment_id
       AND workspaces.id = locked_workspace.id
    RETURNING workspaces.id
),
cancelled_requested_operations AS (
    UPDATE workspace_operations
       SET state = 'cancelled',
           error = jsonb_build_object('code', 'workspace_mount_stopped'),
           completed_at = now(),
           updated_at = now()
      FROM requested_without_runtime
     WHERE workspace_operations.org_id = requested_without_runtime.org_id
       AND workspace_operations.worker_group_id = requested_without_runtime.worker_group_id
       AND workspace_operations.workspace_mount_id = requested_without_runtime.id
       AND workspace_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_operations.id
),
terminated_requested_execs AS (
    UPDATE workspace_execs
       SET state = 'terminated',
           error = jsonb_build_object('code', 'workspace_mount_stopped'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM requested_without_runtime
     WHERE workspace_execs.org_id = requested_without_runtime.org_id
       AND workspace_execs.worker_group_id = requested_without_runtime.worker_group_id
       AND workspace_execs.project_id = requested_without_runtime.project_id
       AND workspace_execs.environment_id = requested_without_runtime.environment_id
       AND workspace_execs.workspace_id = requested_without_runtime.workspace_id
       AND workspace_execs.workspace_mount_id = requested_without_runtime.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
closed_requested_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_mount_stopped'),
           resize_cols = NULL,
           resize_rows = NULL,
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM requested_without_runtime
     WHERE workspace_pty_sessions.org_id = requested_without_runtime.org_id
       AND workspace_pty_sessions.worker_group_id = requested_without_runtime.worker_group_id
       AND workspace_pty_sessions.project_id = requested_without_runtime.project_id
       AND workspace_pty_sessions.environment_id = requested_without_runtime.environment_id
       AND workspace_pty_sessions.workspace_id = requested_without_runtime.workspace_id
       AND workspace_pty_sessions.workspace_mount_id = requested_without_runtime.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
released_requested_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM requested_without_runtime
     WHERE workspace_leases.org_id = requested_without_runtime.org_id
       AND workspace_leases.worker_group_id = requested_without_runtime.worker_group_id
       AND workspace_leases.project_id = requested_without_runtime.project_id
       AND workspace_leases.environment_id = requested_without_runtime.environment_id
       AND workspace_leases.workspace_id = requested_without_runtime.workspace_id
       AND workspace_leases.workspace_mount_id = requested_without_runtime.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
requested_stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT terminated_requested_execs.org_id,
           terminated_requested_execs.project_id,
           terminated_requested_execs.environment_id,
           terminated_requested_execs.workspace_id,
           'workspace_exec'::workspace_resource_kind,
           terminated_requested_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'::workspace_stream_notification_kind
      FROM terminated_requested_execs
      CROSS JOIN LATERAL (VALUES ('stdout', terminated_requested_execs.stdout_cursor), ('stderr', terminated_requested_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT closed_requested_ptys.org_id,
           closed_requested_ptys.project_id,
           closed_requested_ptys.environment_id,
           closed_requested_ptys.workspace_id,
           'workspace_pty'::workspace_resource_kind,
           closed_requested_ptys.id,
           'output',
           closed_requested_ptys.output_cursor,
           'terminal'::workspace_stream_notification_kind
      FROM closed_requested_ptys
    RETURNING id
),
requested_cleanup_counts AS (
    SELECT (SELECT count(*) FROM released_prepared_runtime_reservation)
         + (SELECT count(*) FROM cancelled_requested_operations)
         + (SELECT count(*) FROM released_requested_leases)
         + (SELECT count(*) FROM requested_stream_wakeups) AS count
)
SELECT *
  FROM requested_without_runtime
 WHERE (SELECT count FROM requested_cleanup_counts) >= 0
UNION ALL
SELECT * FROM requested_live_stop
LIMIT 1;

-- name: PromoteWorkspaceMountStopCapture :one
WITH target AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM workspace_mounts
      JOIN workspaces
        ON workspaces.org_id = workspace_mounts.org_id
       AND workspaces.project_id = workspace_mounts.project_id
       AND workspaces.environment_id = workspace_mounts.environment_id
       AND workspaces.id = workspace_mounts.workspace_id
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
      JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                           AND worker_instances.worker_group_id = runtime_instances.worker_group_id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
       ON project_worker_group_placement.org_id = workspace_mounts.org_id
       AND project_worker_group_placement.project_id = workspace_mounts.project_id
       AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
       AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.id = sqlc.arg(id)
       AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
       AND workspace_mounts.state = 'unmounting'
       AND workspace_mounts.dirty_generation > 0
       AND workspaces.current_version_id IS NOT DISTINCT FROM workspace_mounts.base_version_id
     FOR UPDATE OF workspaces, workspace_mounts
),
verified_artifact AS (
    SELECT artifacts.id
      FROM artifacts
      JOIN target
        ON target.org_id = artifacts.org_id
       AND target.project_id = artifacts.project_id
       AND target.environment_id = artifacts.environment_id
      JOIN cas_objects
        ON cas_objects.org_id = artifacts.org_id
       AND cas_objects.digest = artifacts.digest
     WHERE artifacts.org_id = sqlc.arg(org_id)
       AND artifacts.project_id = sqlc.arg(project_id)
       AND artifacts.environment_id = sqlc.arg(environment_id)
       AND artifacts.id = sqlc.arg(artifact_id)
       AND artifacts.kind = 'workspace_version'
       AND artifacts.size_bytes = sqlc.arg(size_bytes)
       AND artifacts.media_type = 'application/vnd.helmr.workspace.v0.tar'
       AND cas_objects.size_bytes = artifacts.size_bytes
       AND cas_objects.media_type = artifacts.media_type
       AND btrim(sqlc.arg(artifact_encoding)::text) <> ''
       AND btrim(sqlc.arg(content_digest)::text) <> ''
),
created_version AS (
    INSERT INTO workspace_versions (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        workspace_id,
        parent_version_id,
        source_workspace_mount_id,
        kind,
        state,
        artifact_id,
        artifact_encoding,
        artifact_entry_count,
        content_digest,
        size_bytes,
        message,
        promoted_at,
        created_by_subject_type
    )
    SELECT sqlc.arg(version_id),
           sqlc.arg(version_public_id)::text,
           target.org_id,
           target.project_id,
           target.environment_id,
           target.workspace_id,
           target.base_version_id,
           target.id,
           'system',
           'ready',
           sqlc.arg(artifact_id),
           sqlc.arg(artifact_encoding),
           sqlc.arg(artifact_entry_count),
           sqlc.arg(content_digest),
           sqlc.arg(size_bytes),
           sqlc.arg(message),
           now(),
           'worker'
      FROM target
      JOIN verified_artifact ON verified_artifact.id = sqlc.arg(artifact_id)
    RETURNING *
),
promoted_workspace AS (
    UPDATE workspaces
       SET current_version_id = created_version.id,
           dirty_state = 'clean',
           updated_at = now()
      FROM created_version
     WHERE workspaces.org_id = created_version.org_id
       AND workspaces.project_id = created_version.project_id
       AND workspaces.environment_id = created_version.environment_id
       AND workspaces.id = created_version.workspace_id
       AND workspaces.current_version_id IS NOT DISTINCT FROM created_version.parent_version_id
    RETURNING workspaces.id
),
promoted_mount AS (
    UPDATE workspace_mounts
       SET dirty_generation = 0,
           updated_at = now()
      FROM created_version
     WHERE workspace_mounts.org_id = created_version.org_id
       AND workspace_mounts.id = created_version.source_workspace_mount_id
       AND workspace_mounts.state = 'unmounting'
    RETURNING workspace_mounts.id
)
SELECT created_version.*
  FROM created_version
  JOIN promoted_workspace ON promoted_workspace.id = created_version.workspace_id
  JOIN promoted_mount ON promoted_mount.id = created_version.source_workspace_mount_id;

-- name: StopWorkspaceMount :one
WITH target AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM workspace_mounts
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
       AND (
            (
                workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
                AND runtime_instances.id = workspace_mounts.runtime_instance_id
            )
            OR (
                workspace_mounts.state = 'unmounted'
                AND workspace_mounts.runtime_instance_id IS NULL
                AND workspace_mounts.stopped_at IS NOT NULL
                AND runtime_instances.workspace_mount_id = workspace_mounts.id
                AND runtime_instances.state IN ('closed', 'lost', 'failed')
            )
       )
      JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                           AND worker_instances.worker_group_id = runtime_instances.worker_group_id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
       ON project_worker_group_placement.org_id = workspace_mounts.org_id
       AND project_worker_group_placement.project_id = workspace_mounts.project_id
       AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
       AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.id = sqlc.arg(id)
),
stopped AS (
    UPDATE workspace_mounts
       SET state = 'unmounted',
           unmounted_at = coalesce(workspace_mounts.unmounted_at, now()),
           stopped_at = coalesce(workspace_mounts.stopped_at, now()),
           runtime_instance_id = NULL,
           updated_at = now()
      FROM target
     WHERE workspace_mounts.org_id = target.org_id
       AND workspace_mounts.id = target.id
       AND target.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.*
),
replayed AS (
    SELECT target.*
      FROM target
     WHERE target.state = 'unmounted'
),
cancelled_operations AS (
    UPDATE workspace_operations
       SET state = 'cancelled',
           error = jsonb_build_object('code', 'workspace_mount_stopped'),
           completed_at = now(),
           updated_at = now()
      FROM stopped
     WHERE workspace_operations.org_id = stopped.org_id
       AND workspace_operations.workspace_mount_id = stopped.id
       AND workspace_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_operations.id
),
terminated_execs AS (
    UPDATE workspace_execs
       SET state = 'terminated',
           error = jsonb_build_object('code', 'workspace_mount_stopped'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM stopped
     WHERE workspace_execs.org_id = stopped.org_id
       AND workspace_execs.project_id = stopped.project_id
       AND workspace_execs.environment_id = stopped.environment_id
       AND workspace_execs.workspace_id = stopped.workspace_id
       AND workspace_execs.workspace_mount_id = stopped.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
closed_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_mount_stopped'),
           resize_cols = NULL,
           resize_rows = NULL,
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM stopped
     WHERE workspace_pty_sessions.org_id = stopped.org_id
       AND workspace_pty_sessions.project_id = stopped.project_id
       AND workspace_pty_sessions.environment_id = stopped.environment_id
       AND workspace_pty_sessions.workspace_id = stopped.workspace_id
       AND workspace_pty_sessions.workspace_mount_id = stopped.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
released_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM stopped
     WHERE workspace_leases.org_id = stopped.org_id
       AND workspace_leases.project_id = stopped.project_id
       AND workspace_leases.environment_id = stopped.environment_id
       AND workspace_leases.workspace_id = stopped.workspace_id
       AND workspace_leases.workspace_mount_id = stopped.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
updated_workspace AS (
    UPDATE workspaces
       SET desired_state = 'stopped',
           updated_at = now()
      FROM stopped
     WHERE workspaces.org_id = stopped.org_id
       AND workspaces.project_id = stopped.project_id
       AND workspaces.environment_id = stopped.environment_id
       AND workspaces.id = stopped.workspace_id
    RETURNING workspaces.id
),
closed_runtime_instances AS (
    UPDATE runtime_instances
       SET state = 'closed',
           closed_at = coalesce(runtime_instances.closed_at, now()),
           expires_at = NULL,
           owner_workspace_id = NULL,
           owner_workspace_version_id = NULL,
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_run_state_version = NULL,
           updated_at = now()
      FROM stopped
     WHERE runtime_instances.org_id = stopped.org_id
       AND runtime_instances.workspace_mount_id = stopped.id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
       AND runtime_instances.state IN ('binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
    RETURNING runtime_instances.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT terminated_execs.org_id,
           terminated_execs.project_id,
           terminated_execs.environment_id,
           terminated_execs.workspace_id,
           'workspace_exec'::workspace_resource_kind,
           terminated_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'::workspace_stream_notification_kind
      FROM terminated_execs
      CROSS JOIN LATERAL (VALUES ('stdout', terminated_execs.stdout_cursor), ('stderr', terminated_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT closed_ptys.org_id,
           closed_ptys.project_id,
           closed_ptys.environment_id,
           closed_ptys.workspace_id,
           'workspace_pty'::workspace_resource_kind,
           closed_ptys.id,
           'output',
           closed_ptys.output_cursor,
           'terminal'::workspace_stream_notification_kind
      FROM closed_ptys
    RETURNING id
)
SELECT *
  FROM stopped
 WHERE (SELECT count(*) FROM stream_wakeups)
     + (SELECT count(*) FROM closed_runtime_instances)
     + (SELECT count(*) FROM cancelled_operations)
     + (SELECT count(*) FROM released_leases)
     + (SELECT count(*) FROM updated_workspace) >= 0
UNION ALL
SELECT *
  FROM replayed
 WHERE NOT EXISTS (SELECT 1 FROM stopped);

-- name: RequestCapacityPressureIdleWorkspaceMountStopsForWorker :many
WITH worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.status = 'active'
),
victim AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM workspace_mounts
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.workspace_mount_id = workspace_mounts.id
       AND runtime_instances.state = 'waiting_hot'
      JOIN workspaces
        ON workspaces.org_id = workspace_mounts.org_id
       AND workspaces.project_id = workspace_mounts.project_id
       AND workspaces.environment_id = workspace_mounts.environment_id
       AND workspaces.id = workspace_mounts.workspace_id
      JOIN worker_scope ON true
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
       ON project_worker_group_placement.org_id = workspace_mounts.org_id
       AND project_worker_group_placement.project_id = workspace_mounts.project_id
       AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
       AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
       AND project_worker_group_placement.worker_group_id = worker_scope.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE workspace_mounts.state = 'mounted'
       AND workspace_mounts.dirty_generation = 0
       AND workspaces.state = 'active'
       AND workspaces.deleted_at IS NULL
       AND NOT EXISTS (
           SELECT 1
             FROM runs
             JOIN run_leases ON run_leases.org_id = runs.org_id
                            AND run_leases.run_id = runs.id
            WHERE runs.org_id = workspace_mounts.org_id
              AND runs.workspace_mount_id = workspace_mounts.id
              AND run_leases.worker_instance_id = runtime_instances.worker_instance_id
              AND run_leases.status IN ('leased', 'running')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.org_id = workspace_mounts.org_id
              AND workspace_leases.project_id = workspace_mounts.project_id
              AND workspace_leases.environment_id = workspace_mounts.environment_id
              AND workspace_leases.workspace_id = workspace_mounts.workspace_id
              AND workspace_leases.workspace_mount_id = workspace_mounts.id
              AND workspace_leases.state IN ('active', 'releasing')
              AND workspace_leases.expires_at > now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_execs
            WHERE workspace_execs.org_id = workspace_mounts.org_id
              AND workspace_execs.project_id = workspace_mounts.project_id
              AND workspace_execs.environment_id = workspace_mounts.environment_id
              AND workspace_execs.workspace_id = workspace_mounts.workspace_id
              AND (workspace_execs.workspace_mount_id = workspace_mounts.id OR workspace_execs.workspace_mount_id IS NULL)
              AND workspace_execs.state IN ('queued', 'materializing', 'running')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_pty_sessions
            WHERE workspace_pty_sessions.org_id = workspace_mounts.org_id
              AND workspace_pty_sessions.project_id = workspace_mounts.project_id
              AND workspace_pty_sessions.environment_id = workspace_mounts.environment_id
              AND workspace_pty_sessions.workspace_id = workspace_mounts.workspace_id
              AND (workspace_pty_sessions.workspace_mount_id = workspace_mounts.id OR workspace_pty_sessions.workspace_mount_id IS NULL)
              AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
       )
     ORDER BY workspaces.last_activity_at ASC,
              runtime_instances.waiting_at ASC NULLS LAST,
              workspace_mounts.mounted_at ASC,
              workspace_mounts.id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF workspace_mounts, runtime_instances, workspaces SKIP LOCKED
),
stopping AS (
    UPDATE workspace_mounts
       SET state = 'unmounting',
           updated_at = now()
      FROM victim
     WHERE workspace_mounts.org_id = victim.org_id
       AND workspace_mounts.id = victim.id
    RETURNING workspace_mounts.*
),
stopping_runtime_instances AS (
    UPDATE runtime_instances
       SET state = 'stopping',
           stopping_requested_at = COALESCE(runtime_instances.stopping_requested_at, now()),
           last_reclaim_reason = 'capacity_pressure',
           updated_at = now()
      FROM stopping
     WHERE runtime_instances.org_id = stopping.org_id
       AND runtime_instances.id = stopping.runtime_instance_id
       AND runtime_instances.workspace_mount_id = stopping.id
       AND runtime_instances.state = 'waiting_hot'
    RETURNING runtime_instances.id
),
updated_workspaces AS (
    UPDATE workspaces
       SET desired_state = 'stopped',
           updated_at = now()
      FROM stopping
     WHERE workspaces.org_id = stopping.org_id
       AND workspaces.project_id = stopping.project_id
       AND workspaces.environment_id = stopping.environment_id
       AND workspaces.id = stopping.workspace_id
    RETURNING workspaces.id
)
SELECT stopping.*
  FROM stopping
 WHERE (SELECT count(*) FROM stopping_runtime_instances)
     + (SELECT count(*) FROM updated_workspaces) >= 0;

-- name: FailWorkspaceMount :one
WITH target AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM workspace_mounts
      JOIN workspaces
        ON workspaces.org_id = workspace_mounts.org_id
       AND workspaces.project_id = workspace_mounts.project_id
       AND workspaces.environment_id = workspace_mounts.environment_id
       AND workspaces.id = workspace_mounts.workspace_id
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.worker_group_id = workspace_mounts.worker_group_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
      JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                           AND worker_instances.worker_group_id = runtime_instances.worker_group_id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
       ON project_worker_group_placement.org_id = workspace_mounts.org_id
       AND project_worker_group_placement.project_id = workspace_mounts.project_id
       AND project_worker_group_placement.environment_id = workspace_mounts.environment_id
       AND project_worker_group_placement.worker_group_id = workspace_mounts.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.id = sqlc.arg(id)
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
     FOR UPDATE OF workspaces, workspace_mounts
),
failed AS (
    UPDATE workspace_mounts
       SET state = 'failed',
           failed_at = now(),
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           updated_at = now()
      FROM target
     WHERE workspace_mounts.org_id = target.org_id
       AND workspace_mounts.id = target.id
    RETURNING workspace_mounts.*
),
failed_runtime_instances AS (
    UPDATE runtime_instances
       SET state = 'failed',
           failed_at = coalesce(runtime_instances.failed_at, now()),
           expires_at = NULL,
           owner_workspace_id = NULL,
           owner_workspace_version_id = NULL,
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_run_state_version = NULL,
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           updated_at = now()
      FROM target
     WHERE runtime_instances.id = target.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.state IN ('binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
    RETURNING runtime_instances.id
),
updated_workspace AS (
    UPDATE workspaces
       SET state = CASE
               WHEN target.dirty_generation > 0 AND target.state = 'unmounting' THEN 'recovery_required'::workspace_state
               ELSE workspaces.state
           END,
           dirty_state = CASE
               WHEN target.dirty_generation > 0 AND target.state = 'unmounting' THEN 'capture_failed'::workspace_dirty_state
               ELSE workspaces.dirty_state
           END,
           updated_at = now()
      FROM target
     WHERE workspaces.org_id = target.org_id
       AND workspaces.project_id = target.project_id
       AND workspaces.environment_id = target.environment_id
       AND workspaces.id = target.workspace_id
    RETURNING workspaces.id
),
lost_operations AS (
    UPDATE workspace_operations
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_mount_failed'),
           completed_at = now(),
           updated_at = now()
      FROM failed
     WHERE workspace_operations.org_id = failed.org_id
       AND workspace_operations.workspace_mount_id = failed.id
       AND workspace_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_operations.id
),
lost_execs AS (
    UPDATE workspace_execs
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_mount_failed'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM failed
     WHERE workspace_execs.org_id = failed.org_id
       AND workspace_execs.project_id = failed.project_id
       AND workspace_execs.environment_id = failed.environment_id
       AND workspace_execs.workspace_id = failed.workspace_id
       AND workspace_execs.workspace_mount_id = failed.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
lost_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_mount_failed'),
           resize_cols = NULL,
           resize_rows = NULL,
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM failed
     WHERE workspace_pty_sessions.org_id = failed.org_id
       AND workspace_pty_sessions.project_id = failed.project_id
       AND workspace_pty_sessions.environment_id = failed.environment_id
       AND workspace_pty_sessions.workspace_id = failed.workspace_id
       AND workspace_pty_sessions.workspace_mount_id = failed.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
released_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM failed
     WHERE workspace_leases.org_id = failed.org_id
       AND workspace_leases.project_id = failed.project_id
       AND workspace_leases.environment_id = failed.environment_id
       AND workspace_leases.workspace_id = failed.workspace_id
       AND workspace_leases.workspace_mount_id = failed.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT lost_execs.org_id,
           lost_execs.project_id,
           lost_execs.environment_id,
           lost_execs.workspace_id,
           'workspace_exec'::workspace_resource_kind,
           lost_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'::workspace_stream_notification_kind
      FROM lost_execs
      CROSS JOIN LATERAL (VALUES ('stdout', lost_execs.stdout_cursor), ('stderr', lost_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT lost_ptys.org_id,
           lost_ptys.project_id,
           lost_ptys.environment_id,
           lost_ptys.workspace_id,
           'workspace_pty'::workspace_resource_kind,
           lost_ptys.id,
           'output',
           lost_ptys.output_cursor,
           'terminal'::workspace_stream_notification_kind
      FROM lost_ptys
    RETURNING id
)
SELECT *
 FROM failed
 WHERE (SELECT count(*) FROM stream_wakeups)
     + (SELECT count(*) FROM failed_runtime_instances)
     + (SELECT count(*) FROM updated_workspace)
     + (SELECT count(*) FROM lost_operations)
     + (SELECT count(*) FROM released_leases) >= 0;

-- name: MarkStaleWorkspaceMountsLost :many
WITH target AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM workspace_mounts
     WHERE workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
       AND workspace_mounts.last_heartbeat_at < sqlc.arg(stale_before)
       AND NOT EXISTS (
           SELECT 1
             FROM runs
             JOIN run_leases ON run_leases.org_id = runs.org_id
                            AND run_leases.run_id = runs.id
            WHERE runs.org_id = workspace_mounts.org_id
              AND runs.workspace_mount_id = workspace_mounts.id
              AND run_leases.status IN ('leased', 'running')
       )
),
lost AS (
    UPDATE workspace_mounts
       SET state = 'lost',
           lost_at = now(),
           updated_at = now()
      FROM target
     WHERE workspace_mounts.org_id = target.org_id
       AND workspace_mounts.id = target.id
    RETURNING workspace_mounts.*
),
lost_runtime_instances AS (
    UPDATE runtime_instances
       SET state = 'lost',
           lost_at = coalesce(runtime_instances.lost_at, now()),
           expires_at = NULL,
           owner_workspace_id = NULL,
           owner_workspace_version_id = NULL,
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_run_state_version = NULL,
           adopting_workspace_mount_id = NULL,
           adoption_expires_at = NULL,
           updated_at = now()
      FROM target
     WHERE runtime_instances.id = target.runtime_instance_id
       AND runtime_instances.workspace_mount_id = target.id
       AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
    RETURNING runtime_instances.id
),
lost_operations AS (
    UPDATE workspace_operations
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_mount_lost'),
           completed_at = now(),
           updated_at = now()
      FROM lost
     WHERE workspace_operations.org_id = lost.org_id
       AND workspace_operations.workspace_mount_id = lost.id
       AND workspace_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_operations.id
),
updated_lost_dirty_workspaces AS (
    UPDATE workspaces
       SET state = 'recovery_required',
           dirty_state = 'dirty_state_lost',
           updated_at = now()
      FROM lost
     WHERE lost.dirty_generation > 0
       AND workspaces.org_id = lost.org_id
       AND workspaces.project_id = lost.project_id
       AND workspaces.environment_id = lost.environment_id
       AND workspaces.id = lost.workspace_id
       AND workspaces.state = 'active'
       AND workspaces.archived_at IS NULL
       AND workspaces.deleted_at IS NULL
    RETURNING workspaces.id
),
lost_execs AS (
    UPDATE workspace_execs
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_mount_lost'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM lost
     WHERE workspace_execs.org_id = lost.org_id
       AND workspace_execs.project_id = lost.project_id
       AND workspace_execs.environment_id = lost.environment_id
       AND workspace_execs.workspace_id = lost.workspace_id
       AND workspace_execs.workspace_mount_id = lost.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
lost_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_mount_lost'),
           resize_cols = NULL,
           resize_rows = NULL,
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM lost
     WHERE workspace_pty_sessions.org_id = lost.org_id
       AND workspace_pty_sessions.project_id = lost.project_id
       AND workspace_pty_sessions.environment_id = lost.environment_id
       AND workspace_pty_sessions.workspace_id = lost.workspace_id
       AND workspace_pty_sessions.workspace_mount_id = lost.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
released_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM lost
     WHERE workspace_leases.org_id = lost.org_id
       AND workspace_leases.project_id = lost.project_id
       AND workspace_leases.environment_id = lost.environment_id
       AND workspace_leases.workspace_id = lost.workspace_id
       AND workspace_leases.workspace_mount_id = lost.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT lost_execs.org_id,
           lost_execs.project_id,
           lost_execs.environment_id,
           lost_execs.workspace_id,
           'workspace_exec'::workspace_resource_kind,
           lost_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'::workspace_stream_notification_kind
      FROM lost_execs
      CROSS JOIN LATERAL (VALUES ('stdout', lost_execs.stdout_cursor), ('stderr', lost_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT lost_ptys.org_id,
           lost_ptys.project_id,
           lost_ptys.environment_id,
           lost_ptys.workspace_id,
           'workspace_pty'::workspace_resource_kind,
           lost_ptys.id,
           'output',
           lost_ptys.output_cursor,
           'terminal'::workspace_stream_notification_kind
      FROM lost_ptys
    RETURNING id
)
SELECT *
  FROM lost
 WHERE (SELECT count(*) FROM stream_wakeups)
     + (SELECT count(*) FROM lost_runtime_instances)
     + (SELECT count(*) FROM lost_operations)
     + (SELECT count(*) FROM updated_lost_dirty_workspaces)
     + (SELECT count(*) FROM released_leases) >= 0;
