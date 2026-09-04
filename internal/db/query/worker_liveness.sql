-- name: ListStaleWorkerFenceCandidates :many
SELECT workers.id,
       workers.worker_group_id,
       workers.current_epoch,
       workers.state,
       COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at) AS freshness_at,
       CASE
           WHEN workers.state = 'registering' AND workers.observed_at IS NULL
               THEN 'registering_observation_missing'
           ELSE 'worker_observation_stale'
       END::text AS reason
  FROM worker_instances AS workers
 WHERE workers.state IN ('registering', 'active', 'draining')
   AND (sqlc.narg(worker_group_id)::uuid IS NULL OR workers.worker_group_id = sqlc.narg(worker_group_id))
   AND (
       (workers.state = 'registering'
        AND workers.observed_at IS NULL
        AND COALESCE(workers.epoch_started_at, workers.updated_at)
            < sqlc.arg(registration_stale_before))
       OR
       (workers.state IN ('active', 'draining')
        AND COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at)
            < transaction_timestamp()
                - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second')
   )
 ORDER BY COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at),
          workers.id
 LIMIT sqlc.arg(row_limit)
 FOR UPDATE OF workers SKIP LOCKED;

-- name: RecheckAndFenceStaleWorkerInstance :one
WITH target AS (
    UPDATE worker_instances AS workers
       SET state = 'lost',
           claim_version = workers.claim_version + 1,
           lost_at = COALESCE(workers.lost_at, now()),
           updated_at = now()
     WHERE workers.id = sqlc.arg(id)
       AND workers.worker_group_id = sqlc.arg(worker_group_id)
       AND workers.current_epoch IS NOT DISTINCT FROM sqlc.arg(expected_epoch)
       AND workers.state IN ('registering', 'active', 'draining')
       AND (
           (workers.state = 'registering'
            AND workers.observed_at IS NULL
            AND COALESCE(workers.epoch_started_at, workers.updated_at)
                < sqlc.arg(registration_stale_before))
           OR
           (workers.state IN ('active', 'draining')
            AND COALESCE(workers.observed_at, workers.activated_at, workers.epoch_started_at, workers.updated_at)
                < transaction_timestamp()
                    - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second')
       )
    RETURNING workers.*
), revoked_credentials AS (
    UPDATE worker_instance_credentials AS credentials
       SET revoked_at = COALESCE(credentials.revoked_at, now())
      FROM target
     WHERE credentials.worker_instance_id = target.id
       AND credentials.revoked_at IS NULL
    RETURNING credentials.id
), lost_mounts AS (
    UPDATE workspace_mounts AS mounts
       SET state = 'lost', lost_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE mounts.worker_instance_id = target.id
       AND mounts.worker_epoch = target.current_epoch
       AND mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING mounts.id
), lost_runtimes AS (
    UPDATE runtime_instances AS runtimes
       SET observed_state = 'lost', observed_version = runtimes.observed_version + 1,
           observed_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code),
           reserved_run_id = NULL, reserved_attempt_number = NULL,
           reserved_process_id = NULL, reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL, updated_at = now()
      FROM target
     WHERE runtimes.worker_instance_id = target.id
       AND runtimes.worker_epoch = target.current_epoch
       AND runtimes.reclaimed_at IS NULL
       AND runtimes.observed_state IN ('allocated', 'ready')
    RETURNING runtimes.id
)
-- Immediate fencing revokes credentials and terminalizes mount/runtime
-- observations. Run/build/workspace authority is recovered by its canonical
-- expiry and recovery loops; this transition does not imply zero authority.
SELECT target.id, target.worker_group_id, target.current_epoch, target.state
  FROM target
 WHERE (SELECT count(*) FROM revoked_credentials) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0;
