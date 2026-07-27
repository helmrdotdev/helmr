-- name: GetNextRuntimeReconcileTarget :one
SELECT runtime_instances.*,
       worker_network_slots.id AS network_slot_id,
       worker_network_slots.generation AS network_slot_generation,
       artifacts.digest AS workspace_image_digest,
       artifacts.size_bytes AS workspace_image_size_bytes,
       artifacts.media_type AS workspace_image_media_type,
       deployment_definitions.workspace_architecture,
       '/workspace'::text AS workspace_mount_path,
       reserved_workspace_versions.id AS base_workspace_version_id,
       reserved_workspace_versions.entry_count AS workspace_entry_count,
       COALESCE(reserved_workspace_artifacts.digest, '') AS workspace_artifact_digest,
       COALESCE(reserved_workspace_artifacts.size_bytes, 0) AS workspace_artifact_size_bytes,
       COALESCE(reserved_workspace_artifacts.media_type, '') AS workspace_artifact_media_type,
       program_deployments.id AS program_deployment_authority_id,
       program_deployments.program_runtime_digest,
       program_deployments.program_architecture,
       program_deployments.build_contract_version AS program_build_contract_version,
       COALESCE(
           program_deployments.program_receipt #>> '{program,indexDigest}',
           ''
       )::text AS program_index_digest,
       COALESCE(program_artifact.digest, '') AS program_artifact_digest,
       COALESCE(program_artifact.size_bytes, 0) AS program_artifact_size_bytes,
       COALESCE(program_artifact.media_type, '') AS program_artifact_media_type,
       runtime_identities.rootfs_digest,
       runtime_identities.runtime_abi,
       runtime_substrates.substrate_digest,
       runtime_substrates.substrate_format,
       runtime_substrates.builder_abi,
       runtime_substrates.layout_abi,
       runtime_substrates.substrate_size_bytes,
       COALESCE(substrate_artifacts.digest, '') AS runtime_substrate_blob_digest,
       COALESCE(substrate_artifacts.size_bytes, 0) AS runtime_substrate_blob_size_bytes,
       COALESCE(substrate_artifacts.media_type, '') AS runtime_substrate_blob_media_type
  FROM runtime_instances
  JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                       AND worker_instances.worker_group_id = runtime_instances.worker_group_id
  JOIN runtime_identities ON runtime_identities.id = runtime_instances.runtime_identity_id
  JOIN worker_network_slots ON worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
                    AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
                    AND worker_network_slots.runtime_instance_id = runtime_instances.id
                    AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runtime_instances.environment_id
   AND deployment_definitions.id = runtime_instances.deployment_definition_id
   AND deployment_definitions.kind = 'workspace'
  JOIN artifacts ON artifacts.environment_id = deployment_definitions.environment_id
                AND artifacts.id = deployment_definitions.artifact_id
  LEFT JOIN workspace_versions AS reserved_workspace_versions
    ON reserved_workspace_versions.environment_id = runtime_instances.environment_id
   AND reserved_workspace_versions.workspace_id = runtime_instances.workspace_id
   AND reserved_workspace_versions.id = runtime_instances.reserved_workspace_version_id
   AND reserved_workspace_versions.state = CASE
           WHEN runtime_instances.restore_checkpoint_id IS NULL THEN 'committed'
           ELSE 'private'
       END
  LEFT JOIN artifacts AS reserved_workspace_artifacts
    ON reserved_workspace_artifacts.environment_id = reserved_workspace_versions.environment_id
   AND reserved_workspace_artifacts.id = reserved_workspace_versions.artifact_id
  LEFT JOIN deployments AS program_deployments
    ON program_deployments.environment_id = runtime_instances.environment_id
   AND program_deployments.id = runtime_instances.program_deployment_id
   AND program_deployments.status = 'deployed'
  LEFT JOIN artifacts AS program_artifact
    ON program_artifact.environment_id = program_deployments.environment_id
   AND program_artifact.id = program_deployments.program_artifact_id
   AND program_artifact.kind = 'deployment_program'
  LEFT JOIN runtime_substrates ON runtime_substrates.org_id = runtime_instances.org_id
                              AND runtime_substrates.project_id = runtime_instances.project_id
                              AND runtime_substrates.environment_id = runtime_instances.environment_id
                              AND runtime_substrates.id = runtime_instances.runtime_substrate_id
  LEFT JOIN artifacts AS substrate_artifacts
    ON substrate_artifacts.org_id = runtime_substrates.org_id
   AND substrate_artifacts.project_id = runtime_substrates.project_id
   AND substrate_artifacts.environment_id = runtime_substrates.environment_id
   AND substrate_artifacts.id = runtime_substrates.artifact_id
 WHERE runtime_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instances.reclaimed_at IS NULL
   AND worker_instances.current_epoch = runtime_instances.worker_epoch
   AND worker_instances.state IN ('active', 'draining')
   AND (
       (runtime_instances.desired_state = 'ready'
        AND runtime_instances.observed_state IN ('allocated', 'preparing')
        AND runtime_instances.observed_desired_version < runtime_instances.desired_version
        AND worker_instances.state = 'active')
       OR
       (runtime_instances.desired_state = 'closed'
        AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing'))
       OR
       (runtime_instances.observed_state = 'failed'
        AND runtime_instances.reclaimed_at IS NULL
        AND NOT EXISTS (
            SELECT 1
              FROM run_leases
             WHERE run_leases.runtime_instance_id = runtime_instances.id
               AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
        )
        AND worker_network_slots.state IN ('reclaiming', 'quarantined'))
   )
 ORDER BY runtime_instances.desired_at, runtime_instances.id
 LIMIT 1;

-- name: RenewRuntimeInstance :one
UPDATE runtime_instances
   SET observed_at = now(), observed_version = observed_version + 1, updated_at = now()
  FROM worker_network_slots
 WHERE runtime_instances.id = sqlc.arg(id) AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
   AND worker_network_slots.id = sqlc.arg(network_slot_id)
   AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
   AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
   AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.runtime_instance_id = runtime_instances.id
   AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming')
   AND observed_version = sqlc.arg(expected_observed_version)
   AND observed_state IN ('allocated', 'preparing', 'ready', 'closing')
RETURNING runtime_instances.*;

-- name: MarkRuntimeInstanceReady :one
WITH restore_secret_authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id,
           workspace_secrets.placement_kind,
           workspace_secrets.placement_target
      FROM runtime_instances
      JOIN workspace_secrets
        ON workspace_secrets.workspace_id = runtime_instances.workspace_id
      JOIN secrets
        ON secrets.id = workspace_secrets.secret_id
       AND secrets.state = 'active'
      JOIN secret_resolutions
        ON secret_resolutions.workspace_id = workspace_secrets.workspace_id
       AND secret_resolutions.run_id = runtime_instances.reserved_run_id
       AND secret_resolutions.attempt_number = runtime_instances.reserved_attempt_number
       AND secret_resolutions.placement_kind = workspace_secrets.placement_kind
       AND secret_resolutions.placement_target = workspace_secrets.placement_target
       AND secret_resolutions.secret_id = workspace_secrets.secret_id
       AND secret_resolutions.revocation_generation = secrets.revocation_generation
     WHERE runtime_instances.id = sqlc.arg(id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.restore_checkpoint_id IS NOT NULL
     ORDER BY secrets.id, workspace_secrets.placement_kind, workspace_secrets.placement_target
     FOR UPDATE OF secrets
), restore_actor_authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id,
           actors.id AS actor_id,
           actors.committed_input_sequence,
           actors.next_input_sequence
      FROM runtime_instances
      JOIN runs
        ON runs.id = runtime_instances.reserved_run_id
       AND runs.entrypoint_kind = 'actor'
       AND runs.actor_id IS NOT NULL
      JOIN actors
        ON actors.id = runs.actor_id
       AND actors.workspace_id = runtime_instances.workspace_id
       AND actors.current_run_id = runs.id
       AND actors.state IN ('open', 'closing')
     WHERE runtime_instances.id = sqlc.arg(id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.restore_checkpoint_id IS NOT NULL
       AND (SELECT count(*) FROM restore_secret_authority) >= 0
     FOR UPDATE OF actors
), restore_entrypoint_authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id,
           NULL::uuid AS actor_id,
           NULL::bigint AS committed_input_sequence,
           NULL::bigint AS next_input_sequence
      FROM runtime_instances
      JOIN runs ON runs.id = runtime_instances.reserved_run_id
     WHERE runtime_instances.id = sqlc.arg(id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.restore_checkpoint_id IS NOT NULL
       AND runs.entrypoint_kind = 'task'
       AND runs.actor_id IS NULL
       AND (SELECT count(*) FROM restore_secret_authority) >= 0
    UNION ALL
    SELECT restore_actor_authority.* FROM restore_actor_authority
), restore_run_authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id,
           restore_entrypoint_authority.actor_id,
           restore_entrypoint_authority.committed_input_sequence,
           restore_entrypoint_authority.next_input_sequence,
           runs.entrypoint_kind,
           runs.actor_start_input_sequence,
           runs.actor_start_input_high_watermark
      FROM restore_entrypoint_authority
      JOIN runtime_instances
        ON runtime_instances.id = restore_entrypoint_authority.runtime_instance_id
      JOIN runs
        ON runs.id = runtime_instances.reserved_run_id
       AND runs.org_id = runtime_instances.org_id
       AND runs.project_id = runtime_instances.project_id
       AND runs.environment_id = runtime_instances.environment_id
       AND runs.workspace_id = runtime_instances.workspace_id
       AND runs.current_attempt_number = runtime_instances.reserved_attempt_number
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND ((runs.entrypoint_kind = 'task'
             AND runs.actor_id IS NULL
             AND restore_entrypoint_authority.actor_id IS NULL)
            OR (runs.entrypoint_kind = 'actor'
                AND runs.actor_id = restore_entrypoint_authority.actor_id
                AND runs.cause_kind IN ('actor_start', 'continuation')
                AND runs.parent_run_id IS NULL))
     FOR UPDATE OF runs
), restore_workspace_authority AS MATERIALIZED (
    SELECT restore_run_authority.*
      FROM restore_run_authority
      JOIN runtime_instances
        ON runtime_instances.id = restore_run_authority.runtime_instance_id
      JOIN workspaces
        ON workspaces.id = runtime_instances.workspace_id
       AND workspaces.environment_id = runtime_instances.environment_id
       AND ((restore_run_authority.entrypoint_kind = 'task'
             AND workspaces.owner_run_id = runtime_instances.reserved_run_id
             AND workspaces.owner_actor_id IS NULL)
            OR (restore_run_authority.entrypoint_kind = 'actor'
                AND workspaces.owner_actor_id = restore_run_authority.actor_id
                AND workspaces.owner_run_id IS NULL))
       AND workspaces.state = 'active'
       AND workspaces.desired_state = 'active'
       AND workspaces.dirty_state = 'clean'
     FOR UPDATE OF workspaces
), restore_attempt_authority AS MATERIALIZED (
    SELECT restore_workspace_authority.*
      FROM restore_workspace_authority
      JOIN runtime_instances
        ON runtime_instances.id = restore_workspace_authority.runtime_instance_id
      JOIN run_attempts
        ON run_attempts.run_id = runtime_instances.reserved_run_id
       AND run_attempts.number = runtime_instances.reserved_attempt_number
       AND run_attempts.workspace_id = runtime_instances.workspace_id
       AND run_attempts.entrypoint_kind = restore_workspace_authority.entrypoint_kind
       AND run_attempts.terminal_at IS NULL
       AND (restore_workspace_authority.entrypoint_kind = 'task'
            OR (run_attempts.actor_start_input_sequence IS NOT NULL
                AND run_attempts.actor_start_input_sequence = restore_workspace_authority.actor_start_input_sequence
                AND restore_workspace_authority.actor_start_input_sequence
                    <= restore_workspace_authority.actor_start_input_high_watermark
                AND restore_workspace_authority.actor_start_input_sequence
                    <= restore_workspace_authority.committed_input_sequence
                AND restore_workspace_authority.committed_input_sequence
                    < restore_workspace_authority.next_input_sequence))
     FOR UPDATE OF run_attempts
), substrate_authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id
      FROM runtime_instances
      JOIN worker_instances
        ON worker_instances.id = runtime_instances.worker_instance_id
       AND worker_instances.worker_group_id = runtime_instances.worker_group_id
       AND worker_instances.current_epoch = runtime_instances.worker_epoch
       AND worker_instances.state IN ('active', 'draining')
       AND worker_instances.supports_run
       AND worker_instances.certified_at IS NOT NULL
      JOIN runtime_substrates
        ON runtime_substrates.id = sqlc.arg(runtime_substrate_id)
       AND runtime_substrates.org_id = runtime_instances.org_id
       AND runtime_substrates.project_id = runtime_instances.project_id
       AND runtime_substrates.environment_id = runtime_instances.environment_id
       AND runtime_substrates.deployment_definition_id = runtime_instances.deployment_definition_id
       AND runtime_substrates.substrate_format = worker_instances.substrate_format
       AND runtime_substrates.builder_abi = worker_instances.substrate_builder_abi
       AND runtime_substrates.layout_abi = worker_instances.substrate_layout_abi
     WHERE runtime_instances.id = sqlc.arg(id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND (SELECT count(*) FROM restore_secret_authority) >= 0
       AND (runtime_instances.restore_checkpoint_id IS NULL
            OR EXISTS (
                SELECT 1
                  FROM restore_attempt_authority
                 WHERE restore_attempt_authority.runtime_instance_id = runtime_instances.id
            ))
     ORDER BY worker_instances.id, runtime_substrates.id
     FOR UPDATE OF worker_instances, runtime_substrates
), slot_authority AS MATERIALIZED (
    SELECT substrate_authority.runtime_instance_id
      FROM substrate_authority
      JOIN worker_network_slots
        ON worker_network_slots.id = sqlc.arg(network_slot_id)
       AND worker_network_slots.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_network_slots.worker_epoch = sqlc.arg(worker_epoch)
       AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
       AND worker_network_slots.runtime_instance_id = substrate_authority.runtime_instance_id
       AND worker_network_slots.state = 'assigned'
     FOR UPDATE OF worker_network_slots
), runtime_authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id
      FROM slot_authority
      JOIN runtime_instances
        ON runtime_instances.id = slot_authority.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.desired_version = sqlc.arg(desired_version)
       AND runtime_instances.observed_version = sqlc.arg(expected_observed_version)
       AND runtime_instances.observed_state IN ('allocated', 'preparing')
       AND (runtime_instances.runtime_substrate_id IS NULL
            OR runtime_instances.runtime_substrate_id = sqlc.arg(runtime_substrate_id))
     FOR UPDATE OF runtime_instances
), restore_authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id
      FROM runtime_authority
      JOIN runtime_instances
        ON runtime_instances.id = runtime_authority.runtime_instance_id
      JOIN restore_attempt_authority
        ON restore_attempt_authority.runtime_instance_id = runtime_instances.id
      JOIN run_waits
        ON run_waits.run_id = runtime_instances.reserved_run_id
       AND run_waits.attempt_number = runtime_instances.reserved_attempt_number
       AND run_waits.workspace_id = runtime_instances.workspace_id
       AND run_waits.suspension_state = 'resume_pending'
       AND run_waits.resume_writer_generation IS NULL
       AND (
           (run_waits.handoff_runtime_instance_id IS NULL
            AND run_waits.handoff_workspace_mount_id IS NULL
            AND run_waits.handoff_resume_checkpoint_id IS NULL)
           OR
           (run_waits.handoff_runtime_instance_id IS NOT NULL
            AND run_waits.handoff_workspace_mount_id IS NOT NULL
            AND run_waits.handoff_mount_generation IS NOT NULL
            AND run_waits.ownership_generation IS NOT NULL
            AND run_waits.parent_writer_generation IS NOT NULL
            AND run_waits.child_writer_generation IS NOT NULL
            AND run_waits.resume_workspace_version_id
                = runtime_instances.reserved_workspace_version_id
            AND (
                (run_waits.condition_state = 'completed'
                 AND run_waits.handoff_resume_checkpoint_id
                     = runtime_instances.restore_checkpoint_id)
                OR
                (run_waits.condition_state IN ('failed', 'cancelled')
                 AND run_waits.handoff_resume_checkpoint_id IS NULL
                 AND run_waits.suspend_checkpoint_id
                     = runtime_instances.restore_checkpoint_id)
            ))
       )
      JOIN run_checkpoints
        ON run_checkpoints.id = runtime_instances.restore_checkpoint_id
       AND run_checkpoints.id = CASE
               WHEN run_waits.handoff_runtime_instance_id IS NOT NULL
                AND run_waits.condition_state = 'completed'
               THEN run_waits.handoff_resume_checkpoint_id
               ELSE run_waits.suspend_checkpoint_id
           END
       AND run_checkpoints.kind = CASE
               WHEN run_waits.handoff_runtime_instance_id IS NOT NULL
                AND run_waits.condition_state = 'completed'
               THEN 'handoff_resume'::run_checkpoint_kind
               ELSE 'suspend'::run_checkpoint_kind
           END
       AND run_checkpoints.run_id = runtime_instances.reserved_run_id
       AND run_checkpoints.attempt_number = runtime_instances.reserved_attempt_number
       AND run_checkpoints.run_wait_id = run_waits.id
       AND run_checkpoints.workspace_id = runtime_instances.workspace_id
       AND run_checkpoints.private_workspace_version_id = runtime_instances.reserved_workspace_version_id
       AND run_checkpoints.state = 'ready'
       AND ((restore_attempt_authority.entrypoint_kind = 'task'
             AND run_checkpoints.actor_speculative_input_sequence IS NULL)
            OR (restore_attempt_authority.entrypoint_kind = 'actor'
                AND run_checkpoints.actor_speculative_input_sequence
                    BETWEEN restore_attempt_authority.committed_input_sequence
                        AND restore_attempt_authority.next_input_sequence - 1))
       AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > transaction_timestamp())
      JOIN run_leases AS source_lease
        ON source_lease.id = run_checkpoints.source_run_lease_id
       AND source_lease.run_id = run_checkpoints.run_id
       AND source_lease.attempt_number = run_checkpoints.attempt_number
       AND source_lease.workspace_id = run_checkpoints.workspace_id
       AND source_lease.state = 'checkpointed'
      JOIN runtime_instances AS source_runtime
        ON source_runtime.id = source_lease.runtime_instance_id
       AND source_runtime.runtime_identity_id = runtime_instances.runtime_identity_id
       AND source_runtime.runtime_substrate_id = sqlc.arg(runtime_substrate_id)
      JOIN workspace_versions
        ON workspace_versions.workspace_id = runtime_instances.workspace_id
       AND workspace_versions.id = runtime_instances.reserved_workspace_version_id
       AND workspace_versions.state = 'private'
     WHERE runtime_instances.id = sqlc.arg(id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND (SELECT count(*) FROM workspace_secrets
             WHERE workspace_secrets.workspace_id = runtime_instances.workspace_id)
           = (SELECT count(*) FROM restore_secret_authority
               WHERE restore_secret_authority.runtime_instance_id = runtime_instances.id)
     FOR UPDATE OF run_waits, run_checkpoints, workspace_versions
), bound AS (
    UPDATE worker_network_slots
       SET state = 'bound', host_interface_name = sqlc.arg(host_interface_name),
           guest_address = sqlc.arg(guest_address), gateway_address = sqlc.arg(gateway_address),
           subnet = sqlc.arg(subnet), tap_name = sqlc.arg(tap_name),
           netns_name = sqlc.arg(netns_name), guest_mac = sqlc.arg(guest_mac),
           updated_at = now()
      FROM runtime_authority
      JOIN runtime_instances
        ON runtime_instances.id = runtime_authority.runtime_instance_id
     WHERE worker_network_slots.id = sqlc.arg(network_slot_id)
       AND worker_network_slots.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_network_slots.worker_epoch = sqlc.arg(worker_epoch)
       AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
       AND worker_network_slots.runtime_instance_id = sqlc.arg(id)
       AND worker_network_slots.state = 'assigned'
       AND runtime_authority.runtime_instance_id = worker_network_slots.runtime_instance_id
       AND runtime_instances.worker_instance_id = worker_network_slots.worker_instance_id
       AND runtime_instances.worker_epoch = worker_network_slots.worker_epoch
       AND runtime_instances.desired_version = sqlc.arg(desired_version)
       AND runtime_instances.observed_version = sqlc.arg(expected_observed_version)
       AND runtime_instances.observed_state IN ('allocated','preparing')
       AND (runtime_instances.runtime_substrate_id IS NULL
            OR runtime_instances.runtime_substrate_id = sqlc.arg(runtime_substrate_id))
       AND (
           runtime_instances.restore_checkpoint_id IS NULL
           OR EXISTS (
               SELECT 1 FROM restore_authority
                WHERE restore_authority.runtime_instance_id = runtime_instances.id
           )
       )
    RETURNING worker_network_slots.runtime_instance_id
)
UPDATE runtime_instances
   SET runtime_substrate_id = sqlc.arg(runtime_substrate_id),
       observed_state = 'ready', observed_version = observed_version + 1,
       observed_desired_version = sqlc.arg(desired_version), observed_at = now(),
       preparing_at = COALESCE(preparing_at, now()), ready_at = COALESCE(ready_at, now()),
       updated_at = now()
  FROM bound
 WHERE runtime_instances.id = sqlc.arg(id) AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND bound.runtime_instance_id = runtime_instances.id
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch) AND runtime_instances.desired_version = sqlc.arg(desired_version)
   AND runtime_instances.observed_version = sqlc.arg(expected_observed_version)
   AND runtime_instances.observed_state IN ('allocated', 'preparing')
   AND (runtime_instances.runtime_substrate_id IS NULL
        OR runtime_instances.runtime_substrate_id = sqlc.arg(runtime_substrate_id))
RETURNING runtime_instances.*;

-- name: MarkRuntimeInstanceClosed :one
WITH closed AS (
UPDATE runtime_instances
   SET observed_state = 'closed', observed_version = observed_version + 1,
       observed_desired_version = desired_version, observed_at = now(),
       closing_at = COALESCE(closing_at, now()), closed_at = now(),
       terminal_at = now(), terminal_reason_code = sqlc.arg(reason_code),
       terminal_error = NULL, reclaimed_at = now(),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_process_id = NULL, reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL, updated_at = now()
  FROM worker_network_slots
 WHERE runtime_instances.id = sqlc.arg(id) AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instances.desired_state = 'closed' AND runtime_instances.desired_version = sqlc.arg(desired_version)
   AND worker_network_slots.id = sqlc.arg(network_slot_id)
   AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
   AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
   AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.runtime_instance_id = runtime_instances.id
   AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming')
   AND observed_version = sqlc.arg(expected_observed_version)
   AND observed_state IN ('allocated','preparing','ready','closing')
RETURNING runtime_instances.*
), reclaimed AS (
UPDATE worker_network_slots
   SET state = 'available', generation = generation + 1, runtime_instance_id = NULL,
       host_interface_name = NULL, guest_address = NULL, gateway_address = NULL, subnet = NULL,
       tap_name = NULL, netns_name = NULL, guest_mac = NULL,
       reclaiming_at = NULL, quarantined_at = NULL, lost_at = NULL,
       reclaimed_at = now(), reclaim_evidence = sqlc.arg(cleanup_proof)::jsonb,
       state_reason_code = NULL, state_error = NULL, updated_at = now()
  FROM closed
 WHERE worker_network_slots.id = sqlc.arg(network_slot_id)
   AND worker_network_slots.worker_instance_id = closed.worker_instance_id
   AND worker_network_slots.worker_epoch = closed.worker_epoch
   AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.runtime_instance_id = closed.id
RETURNING worker_network_slots.id
)
SELECT closed.* FROM closed JOIN reclaimed ON true;

-- name: MarkRuntimeInstanceFailed :one
WITH failed AS (
UPDATE runtime_instances
   SET observed_state = 'failed', observed_version = observed_version + 1,
       observed_at = now(), failed_at = now(), terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), terminal_error = sqlc.narg(error),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_process_id = NULL, reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL,
       updated_at = now()
  FROM worker_network_slots
 WHERE runtime_instances.id = sqlc.arg(id) AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
   AND worker_network_slots.id = sqlc.arg(network_slot_id)
   AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
   AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
   AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.runtime_instance_id = runtime_instances.id
   AND worker_network_slots.state IN ('assigned', 'bound')
   AND runtime_instances.desired_version = sqlc.arg(desired_version)
   AND observed_version = sqlc.arg(expected_observed_version)
   AND observed_state IN ('allocated','preparing','ready','closing')
RETURNING runtime_instances.*
), quarantined AS (
UPDATE worker_network_slots
   SET state = 'quarantined', reclaiming_at = COALESCE(reclaiming_at, now()),
       quarantined_at = now(), state_reason_code = 'runtime_physical_cleanup_pending',
       state_error = sqlc.narg(error), updated_at = now()
  FROM failed
 WHERE worker_network_slots.id = sqlc.arg(network_slot_id)
   AND worker_network_slots.worker_instance_id = failed.worker_instance_id
   AND worker_network_slots.worker_epoch = failed.worker_epoch
   AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.runtime_instance_id = failed.id
RETURNING worker_network_slots.id
)
SELECT failed.* FROM failed JOIN quarantined ON true;

-- name: ReclaimFailedRuntimeInstance :one
WITH reclaimed_runtime AS (
UPDATE runtime_instances
   SET reclaimed_at = now(),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_process_id = NULL, reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL, updated_at = now()
  FROM worker_network_slots
 WHERE runtime_instances.id = sqlc.arg(id)
   AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instances.desired_version = sqlc.arg(desired_version)
   AND runtime_instances.observed_version = sqlc.arg(expected_observed_version)
   AND runtime_instances.observed_state = 'failed'
   AND runtime_instances.reclaimed_at IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM run_leases
        WHERE run_leases.runtime_instance_id = runtime_instances.id
          AND run_leases.state IN ('assigned', 'starting', 'running')
   )
   AND worker_network_slots.id = sqlc.arg(network_slot_id)
   AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
   AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
   AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.runtime_instance_id = runtime_instances.id
   AND worker_network_slots.state IN ('reclaiming', 'quarantined')
RETURNING runtime_instances.*
), reclaimed_slot AS (
UPDATE worker_network_slots
   SET state = 'available', generation = generation + 1, runtime_instance_id = NULL,
       host_interface_name = NULL, guest_address = NULL, gateway_address = NULL, subnet = NULL,
       tap_name = NULL, netns_name = NULL, guest_mac = NULL,
       reclaiming_at = NULL, quarantined_at = NULL, lost_at = NULL,
       reclaimed_at = now(), reclaim_evidence = sqlc.arg(cleanup_proof)::jsonb,
       state_reason_code = NULL, state_error = NULL, updated_at = now()
  FROM reclaimed_runtime
 WHERE worker_network_slots.id = sqlc.arg(network_slot_id)
   AND worker_network_slots.worker_instance_id = reclaimed_runtime.worker_instance_id
   AND worker_network_slots.worker_epoch = reclaimed_runtime.worker_epoch
   AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.runtime_instance_id = reclaimed_runtime.id
RETURNING worker_network_slots.id
)
SELECT reclaimed_runtime.* FROM reclaimed_runtime JOIN reclaimed_slot ON true;

-- name: ListRuntimeSubstratePrepareTargets :many
SELECT runtime_instances.* FROM runtime_instances
 WHERE worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_substrate_id IS NULL AND observed_state IN ('allocated','preparing')
 ORDER BY allocated_at, id LIMIT sqlc.arg(limit_count);
