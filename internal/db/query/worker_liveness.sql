-- name: ListStaleWorkerFenceCandidates :many
SELECT workers.id,
       workers.worker_group_id,
       workers.current_epoch,
       workers.state,
       COALESCE(observations.observed_at, workers.epoch_started_at, workers.updated_at) AS freshness_at,
       CASE
           WHEN workers.state = 'registering' AND observations.worker_instance_id IS NULL
               THEN 'registering_observation_missing'
           ELSE 'worker_observation_stale'
       END::text AS reason
  FROM worker_instances AS workers
  LEFT JOIN worker_observations AS observations
    ON observations.worker_instance_id = workers.id
   AND observations.worker_epoch = workers.current_epoch
 WHERE workers.state IN ('registering', 'active', 'draining')
   AND (sqlc.arg(worker_group_id)::text = '' OR workers.worker_group_id = sqlc.arg(worker_group_id)::text)
   AND (
       (workers.state = 'registering'
        AND observations.worker_instance_id IS NULL
        AND COALESCE(workers.epoch_started_at, workers.updated_at)
            < sqlc.arg(registration_stale_before))
       OR
       (workers.state IN ('active', 'draining')
        AND COALESCE(observations.observed_at, workers.epoch_started_at, workers.updated_at)
            < sqlc.arg(observation_stale_before))
   )
 ORDER BY COALESCE(observations.observed_at, workers.epoch_started_at, workers.updated_at),
          workers.id
 LIMIT sqlc.arg(row_limit)
 FOR UPDATE OF workers SKIP LOCKED;

-- name: RecheckAndFenceStaleWorkerInstance :one
WITH target AS (
    UPDATE worker_instances AS workers
       SET state = CASE
               WHEN workers.current_epoch IS NULL THEN 'disabled'::worker_instance_state
               ELSE 'lost'::worker_instance_state
           END,
           claim_version = workers.claim_version + 1,
           disabled_at = CASE
               WHEN workers.current_epoch IS NULL THEN COALESCE(workers.disabled_at, now())
               ELSE workers.disabled_at
           END,
           lost_at = CASE
               WHEN workers.current_epoch IS NOT NULL THEN COALESCE(workers.lost_at, now())
               ELSE workers.lost_at
           END,
           updated_at = now()
     WHERE workers.id = sqlc.arg(id)
       AND workers.worker_group_id = sqlc.arg(worker_group_id)
       AND workers.current_epoch IS NOT DISTINCT FROM sqlc.arg(expected_epoch)
       AND workers.state IN ('registering', 'active', 'draining')
       AND (
           (workers.state = 'registering'
            AND NOT EXISTS (
                SELECT 1
                  FROM worker_observations AS observations
                 WHERE observations.worker_instance_id = workers.id
                   AND observations.worker_epoch = workers.current_epoch
            )
            AND COALESCE(workers.epoch_started_at, workers.updated_at)
                < sqlc.arg(registration_stale_before))
           OR
           (workers.state IN ('active', 'draining')
            AND COALESCE(
                    (SELECT observations.observed_at
                       FROM worker_observations AS observations
                      WHERE observations.worker_instance_id = workers.id
                        AND observations.worker_epoch = workers.current_epoch),
                    workers.epoch_started_at,
                    workers.updated_at
                ) < sqlc.arg(observation_stale_before))
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
           observed_at = now(), lost_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE runtimes.worker_instance_id = target.id
       AND runtimes.worker_epoch = target.current_epoch
       AND runtimes.reclaimed_at IS NULL
       AND runtimes.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING runtimes.id
), lost_slots AS (
    UPDATE worker_network_slots AS slots
       SET state = 'lost', generation = slots.generation + 1,
           lost_at = now(), state_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE slots.worker_instance_id = target.id
       AND slots.worker_epoch = target.current_epoch
       AND slots.state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
    RETURNING slots.id
)
-- Immediate fencing revokes credentials and terminalizes mount/runtime/slot
-- observations. Run/build/workspace authority is recovered by its canonical
-- expiry and recovery loops; this transition does not imply zero authority.
SELECT target.id, target.worker_group_id, target.current_epoch, target.state
  FROM target
 WHERE (SELECT count(*) FROM revoked_credentials) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0
   AND (SELECT count(*) FROM lost_slots) >= 0;
