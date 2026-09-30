-- name: ListStaleWorkerFenceCandidates :many
SELECT workers.id,
       workers.worker_group_id,
       workers.current_epoch,
       workers.status,
       COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at) AS freshness_at,
       CASE
           WHEN workers.status = 'registering' AND workers.observed_at IS NULL
               THEN 'registering_observation_missing'
           ELSE 'worker_observation_stale'
       END::text AS reason
  FROM worker_hosts AS workers
 WHERE workers.status IN ('registering', 'active', 'draining')
   AND (sqlc.narg(worker_group_id)::uuid IS NULL OR workers.worker_group_id = sqlc.narg(worker_group_id))
   AND (
       (workers.status = 'registering'
        AND workers.observed_at IS NULL
        AND COALESCE(workers.epoch_started_at, workers.updated_at)
            < sqlc.arg(registration_stale_before))
       OR
       (workers.status IN ('active', 'draining')
        AND COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at)
            < transaction_timestamp()
                - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second')
   )
 ORDER BY COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at),
          workers.id
 LIMIT sqlc.arg(row_limit)
 FOR UPDATE OF workers SKIP LOCKED;

-- name: RecheckAndFenceStaleWorkerHost :one
WITH target AS (
    UPDATE worker_hosts AS workers
       SET status = 'lost',
           claim_version = workers.claim_version + 1,
           lost_at = COALESCE(workers.lost_at, now()),
           updated_at = now()
     WHERE workers.id = sqlc.arg(id)
       AND workers.worker_group_id = sqlc.arg(worker_group_id)
       AND workers.current_epoch IS NOT DISTINCT FROM sqlc.arg(expected_epoch)
       AND workers.status IN ('registering', 'active', 'draining')
       AND (
           (workers.status = 'registering'
            AND workers.observed_at IS NULL
            AND COALESCE(workers.epoch_started_at, workers.updated_at)
                < sqlc.arg(registration_stale_before))
           OR
           (workers.status IN ('active', 'draining')
            AND COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at)
                < transaction_timestamp()
                    - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second')
       )
    RETURNING workers.*
), revoked_credentials AS (
    UPDATE worker_host_credentials AS credentials
       SET revoked_at = COALESCE(credentials.revoked_at, now())
      FROM target
     WHERE credentials.worker_host_id = target.id
       AND credentials.revoked_at IS NULL
    RETURNING credentials.id
), lost_runtimes AS (
    UPDATE computer_instances AS runtimes
       SET observed_state = 'lost', observed_version = runtimes.observed_version + 1,
           observed_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code),
           mount_state='lost', admission_state='closed', updated_at=now()
      FROM target
     WHERE runtimes.worker_host_id = target.id
       AND runtimes.worker_epoch = target.current_epoch
       AND runtimes.reclaimed_at IS NULL
       AND runtimes.observed_state IN ('allocated', 'ready')
    RETURNING runtimes.id
)
-- Immediate fencing revokes credentials and marks Instance observations lost.
-- Physical reclamation still requires independent exclusion evidence. Run/build/computer authority is recovered by its canonical
-- expiry and recovery loops; this transition does not imply zero authority.
SELECT target.id, target.worker_group_id, target.current_epoch, target.status
  FROM target
 WHERE (SELECT count(*) FROM revoked_credentials) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0;

-- Worker Host fence for placement and Computer preparation. The caller
-- (workergroup.LockPlacementSupply) must already hold the Worker Group and Pool share
-- locks and have validated their status. This query locks the Worker Host and
-- checks epoch, observation freshness and platform. Admission also requires an
-- active host without a Run pause; continuation of admitted work accepts a
-- draining host and ignores pauses. admitting reports whether the locked host
-- could admit new work, including the VM pause.
-- name: LockRunEligibleWorkerHost :one
SELECT (worker_hosts.status = 'active' AND worker_hosts.run_paused_reason IS NULL
        AND worker_hosts.vm_paused_reason IS NULL)::boolean AS admitting
  FROM worker_hosts
  LEFT JOIN vm_platforms
    ON vm_platforms.id = worker_hosts.vm_platform_id
 WHERE worker_hosts.id = sqlc.arg(id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)::bigint
   AND (worker_hosts.status = 'active' OR (sqlc.arg(continuation)::boolean AND worker_hosts.status = 'draining'))
   AND worker_hosts.observed_at >= clock_timestamp() - sqlc.arg(worker_freshness_seconds)::bigint * interval '1 second'
   AND (sqlc.arg(continuation)::boolean OR worker_hosts.run_paused_reason IS NULL)
   AND vm_platforms.arch = sqlc.arg(run_architecture)
   AND vm_platforms.contract = sqlc.arg(contract)
 FOR UPDATE OF worker_hosts;
