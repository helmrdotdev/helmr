-- name: GetRunLease :one
SELECT *
  FROM run_leases
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: GetCurrentRunLease :one
SELECT run_leases.*
  FROM runs
  JOIN run_leases
    ON run_leases.run_id = runs.id
   AND run_leases.attempt_number = runs.current_attempt_number
   AND run_leases.workspace_id = runs.workspace_id
   AND run_leases.id = runs.current_run_lease_id
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id);

-- name: DiscoverWorkerRunLeaseWork :many
WITH worker AS (
    SELECT worker_instances.id,
           worker_instances.current_epoch,
           worker_instances.state,
           worker_instances.max_run_consumers
      FROM worker_instances
      JOIN worker_groups
        ON worker_groups.id = worker_instances.worker_group_id
       AND worker_groups.state = 'active'
       AND worker_groups.allows_run
       AND worker_groups.protocol_version = worker_instances.protocol_version
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)::bigint
       AND worker_instances.protocol_version = sqlc.arg(worker_protocol_version)
       AND worker_instances.state IN ('active', 'draining')
       AND worker_instances.supports_run
)
SELECT run_leases.id,
       run_leases.lease_sequence
  FROM worker
  JOIN run_leases
    ON run_leases.worker_instance_id = worker.id
   AND run_leases.worker_epoch = worker.current_epoch
 WHERE run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.state = 'starting' OR worker.state = 'active')
 ORDER BY CASE run_leases.state
              WHEN 'starting' THEN 0
              ELSE 1
          END,
          run_leases.assigned_at,
          run_leases.id
 LIMIT LEAST(sqlc.arg(row_limit)::int, (SELECT max_run_consumers FROM worker));

-- name: GetRunLeaseClaimLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       run_leases.region_id,
       run_leases.runtime_instance_id,
       run_leases.network_slot_id,
       run_leases.network_slot_generation,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id
  FROM run_leases
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_groups.protocol_version = run_leases.worker_protocol_version
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.protocol_version = run_leases.worker_protocol_version
   AND worker_instances.state IN ('active', 'draining')
   AND worker_instances.supports_run
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.state = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.state = 'starting' OR worker_instances.state = 'active');

-- name: MarkRunLeaseStarting :one
UPDATE run_leases
   SET state = 'starting',
       claimed_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND state = 'assigned'
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
RETURNING *;
