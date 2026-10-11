-- name: GetCapacityWorkerHost :one
SELECT id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch,
       draining_at, drain_reason, termination_ready_at, lost_at,
       created_at, updated_at,
       ((SELECT count(*) FROM computer_leases l WHERE l.worker_host_id=worker_hosts.id AND l.fenced_at IS NULL)
        + (SELECT count(*) FROM computer_preparations p WHERE p.worker_host_id=worker_hosts.id AND p.fenced_at IS NULL))::bigint AS unreclaimed_instances,
       (SELECT count(*) FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE l.worker_host_id=worker_hosts.id AND l.fenced_at IS NULL AND p.fenced_at IS NULL)::bigint AS unreconciled_session_processes
  FROM worker_hosts
 WHERE worker_hosts.id = sqlc.arg(worker_host_id);

-- name: ListCapacityWorkerHosts :many
WITH current_instances AS (
    SELECT DISTINCT ON (worker_group_id, resource_id)
           id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch,
           draining_at, drain_reason, termination_ready_at, lost_at,
           created_at, updated_at,
       ((SELECT count(*) FROM computer_leases l WHERE l.worker_host_id=worker_hosts.id AND l.fenced_at IS NULL)
        + (SELECT count(*) FROM computer_preparations p WHERE p.worker_host_id=worker_hosts.id AND p.fenced_at IS NULL))::bigint AS unreclaimed_instances,
       (SELECT count(*) FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE l.worker_host_id=worker_hosts.id AND l.fenced_at IS NULL AND p.fenced_at IS NULL)::bigint AS unreconciled_session_processes
     FROM worker_hosts
     WHERE (sqlc.narg(worker_group_id)::uuid IS NULL OR worker_group_id = sqlc.narg(worker_group_id))
       AND (
           NOT sqlc.arg(has_unreclaimed_instance)::boolean
           OR EXISTS (
               SELECT 1
                 FROM computer_leases
                WHERE computer_leases.worker_host_id = worker_hosts.id
                  AND computer_leases.fenced_at IS NULL
           )
           OR EXISTS (SELECT 1 FROM computer_preparations p WHERE p.worker_host_id=worker_hosts.id AND p.fenced_at IS NULL)
       )
       AND (
           cardinality(sqlc.arg(resource_ids)::text[]) = 0
           OR resource_id = ANY(sqlc.arg(resource_ids)::text[])
       )
     ORDER BY worker_group_id, resource_id,
              (status IN ('registering', 'active', 'draining')) DESC,
              created_at DESC, id DESC
)
SELECT *
  FROM current_instances
 WHERE (
       cardinality(sqlc.arg(statuses)::text[]) = 0
       OR status = ANY(sqlc.arg(statuses)::text[])
   )
 ORDER BY worker_group_id, resource_id
 LIMIT sqlc.arg(row_limit);

-- name: ConfirmWorkerHostProviderAbsent :one
WITH target AS MATERIALIZED (
    SELECT worker_hosts.id
      FROM worker_hosts
     WHERE worker_hosts.id = sqlc.arg(worker_host_id)
       AND worker_hosts.status IN ('registering', 'active', 'draining', 'lost')
     FOR UPDATE
), transitioned AS (
    UPDATE worker_hosts
       SET status = 'lost',
           claim_version = worker_hosts.claim_version
               + CASE WHEN worker_hosts.status = 'lost' THEN 0 ELSE 1 END,
           lost_at = COALESCE(worker_hosts.lost_at, now()),
           updated_at = CASE
               WHEN worker_hosts.status = 'lost' THEN worker_hosts.updated_at
               ELSE now()
           END
      FROM target
     WHERE worker_hosts.id = target.id
    RETURNING worker_hosts.id, worker_hosts.resource_id,
              worker_hosts.worker_group_id, worker_hosts.worker_pool_id,
              worker_hosts.status, worker_hosts.claim_version,
              worker_hosts.current_epoch, worker_hosts.draining_at, worker_hosts.drain_reason,
              worker_hosts.termination_ready_at, worker_hosts.lost_at,
              worker_hosts.created_at, worker_hosts.updated_at
), revoked_host_secrets AS (
    UPDATE worker_host_secrets
       SET revoked_at = COALESCE(worker_host_secrets.revoked_at, now())
      FROM transitioned
     WHERE worker_host_secrets.worker_host_id = transitioned.id
       AND worker_host_secrets.revoked_at IS NULL
    RETURNING worker_host_secrets.id

)
SELECT transitioned.id, transitioned.resource_id,
       transitioned.worker_group_id, transitioned.worker_pool_id,
       transitioned.status, transitioned.claim_version,
       transitioned.current_epoch, transitioned.draining_at, transitioned.drain_reason,
       transitioned.termination_ready_at, transitioned.lost_at,
       transitioned.created_at, transitioned.updated_at
  FROM transitioned
 WHERE (SELECT count(*) FROM revoked_host_secrets) >= 0;
