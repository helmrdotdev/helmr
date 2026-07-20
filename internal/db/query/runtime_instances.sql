-- name: GetNextRuntimeReconcileTarget :one
SELECT runtime_instances.*,
       worker_network_slots.id AS network_slot_id,
       worker_network_slots.generation AS network_slot_generation,
       artifacts.digest AS sandbox_image_artifact_digest_value,
       artifacts.size_bytes AS sandbox_image_artifact_size_bytes,
       artifacts.media_type AS sandbox_image_artifact_media_type,
       '/workspace'::text AS workspace_mount_path,
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
  JOIN worker_network_slots ON worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
                    AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
                    AND worker_network_slots.runtime_instance_id = runtime_instances.id
                    AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
  JOIN artifacts ON artifacts.org_id = runtime_instances.org_id
                AND artifacts.project_id = runtime_instances.project_id
                AND artifacts.environment_id = runtime_instances.environment_id
                AND artifacts.id = runtime_instances.sandbox_image_artifact_id
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runtime_instances.environment_id
   AND deployment_definitions.id = runtime_instances.deployment_definition_id
   AND deployment_definitions.kind = 'workspace'
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
               AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing')
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
WITH bound AS (
    UPDATE worker_network_slots
       SET state = 'bound', host_interface_name = sqlc.arg(host_interface_name),
           guest_address = sqlc.arg(guest_address), gateway_address = sqlc.arg(gateway_address),
           subnet = sqlc.arg(subnet), tap_name = sqlc.arg(tap_name),
           netns_name = sqlc.arg(netns_name), guest_mac = sqlc.arg(guest_mac),
           updated_at = now()
      FROM runtime_instances
     WHERE worker_network_slots.id = sqlc.arg(network_slot_id)
       AND worker_network_slots.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_network_slots.worker_epoch = sqlc.arg(worker_epoch)
       AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
       AND worker_network_slots.runtime_instance_id = sqlc.arg(id)
       AND worker_network_slots.state = 'assigned'
       AND runtime_instances.id = worker_network_slots.runtime_instance_id
       AND runtime_instances.worker_instance_id = worker_network_slots.worker_instance_id
       AND runtime_instances.worker_epoch = worker_network_slots.worker_epoch
       AND runtime_instances.desired_version = sqlc.arg(desired_version)
       AND runtime_instances.observed_version = sqlc.arg(expected_observed_version)
       AND runtime_instances.observed_state IN ('allocated','preparing')
    RETURNING worker_network_slots.runtime_instance_id
)
UPDATE runtime_instances
   SET observed_state = 'ready', observed_version = observed_version + 1,
       observed_desired_version = sqlc.arg(desired_version), observed_at = now(),
       preparing_at = COALESCE(preparing_at, now()), ready_at = COALESCE(ready_at, now()),
       updated_at = now()
  FROM bound
 WHERE runtime_instances.id = sqlc.arg(id) AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND bound.runtime_instance_id = runtime_instances.id
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch) AND runtime_instances.desired_version = sqlc.arg(desired_version)
   AND runtime_instances.observed_version = sqlc.arg(expected_observed_version)
   AND runtime_instances.observed_state IN ('allocated', 'preparing')
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
