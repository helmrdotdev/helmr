-- name: DiscoverWorkerRunLeaseWork :many
SELECT l.id,l.lease_sequence FROM run_leases l
JOIN worker_hosts h ON h.id=l.worker_host_id AND h.worker_group_id=l.worker_group_id AND h.current_epoch=l.worker_epoch
JOIN worker_groups g ON g.id=h.worker_group_id
JOIN computer_instances i ON i.id=l.computer_instance_id AND i.writer_generation=l.writer_generation
WHERE l.worker_host_id=sqlc.arg(worker_host_id) AND l.worker_group_id=sqlc.arg(worker_group_id)
 AND l.worker_epoch=sqlc.arg(worker_epoch) AND h.status IN ('active','draining') AND g.status IN ('active','draining')
 AND i.observed_state='ready' AND i.desired_state='ready' AND i.mount_state='mounted'
 AND i.admission_state<>'restoring'
 AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp()
 AND l.expires_at>clock_timestamp() AND ((l.status IN ('assigned','starting') AND l.start_deadline_at>clock_timestamp()) OR (l.status='running' AND EXISTS(SELECT 1 FROM run_waits w WHERE w.current_run_lease_id=l.id AND w.suspension_status='resuming')))
 ORDER BY CASE l.status WHEN 'starting' THEN 0 ELSE 1 END,l.created_at,l.id LIMIT sqlc.arg(row_limit);

-- name: GetRunLeaseClaimLocators :one
SELECT l.org_id,l.project_id,l.environment_id,l.run_id,l.computer_id,l.attempt_number,
 l.region_id,l.computer_instance_id,l.writer_generation,i.source_checkpoint_id,
 r.session_id,s.run_generation AS actor_run_generation,r.parent_run_id,r.parent_owns_lifecycle,
 parent.session_id AS parent_session_id,parent.current_attempt_number AS parent_attempt_number,
 parent_actor.run_generation AS parent_actor_run_generation,
 wait.id AS run_wait_id,wait.suspend_checkpoint_id
FROM run_leases l JOIN runs r ON r.id=l.run_id AND r.computer_id=l.computer_id
 AND r.current_attempt_number=l.attempt_number AND r.current_run_lease_id=l.id
JOIN worker_hosts h ON h.id=l.worker_host_id AND h.worker_group_id=l.worker_group_id AND h.current_epoch=l.worker_epoch
JOIN worker_groups g ON g.id=l.worker_group_id AND g.region_id=l.region_id
JOIN computer_instances i ON i.id=l.computer_instance_id AND i.computer_id=l.computer_id
 AND i.writer_generation=l.writer_generation AND i.worker_host_id=l.worker_host_id AND i.worker_epoch=l.worker_epoch
LEFT JOIN sessions s ON s.id=r.session_id AND s.environment_id=r.environment_id
LEFT JOIN runs parent ON parent.id=r.parent_run_id AND parent.environment_id=r.environment_id
LEFT JOIN sessions parent_actor ON parent_actor.id=parent.session_id
LEFT JOIN run_waits wait ON wait.run_id=l.run_id AND wait.attempt_number=l.attempt_number
 AND wait.current_run_lease_id=l.id AND wait.suspension_status IN ('hot','checkpointing','resuming')
WHERE l.id=sqlc.arg(id) AND l.lease_sequence=sqlc.arg(lease_sequence)
 AND l.worker_group_id=sqlc.arg(worker_group_id) AND l.worker_host_id=sqlc.arg(worker_host_id)
 AND l.worker_epoch=sqlc.arg(worker_epoch) AND h.status IN ('active','draining') AND g.status IN ('active','draining')
 AND i.desired_state='ready' AND i.observed_state='ready' AND i.mount_state='mounted'
 AND i.writer_expires_at>clock_timestamp() AND i.reclaimed_at IS NULL
 AND l.expires_at>clock_timestamp() AND l.status IN ('assigned','starting') AND r.status IN ('queued','waiting') AND l.start_deadline_at>clock_timestamp();

-- name: GetRunLeaseStartLocators :one
SELECT l.org_id,l.project_id,l.environment_id,l.run_id,l.computer_id,l.attempt_number,
 l.region_id,l.computer_instance_id,l.writer_generation,i.source_checkpoint_id,
 r.session_id,s.run_generation AS actor_run_generation,r.parent_run_id,r.parent_owns_lifecycle,
 parent.session_id AS parent_session_id,parent.current_attempt_number AS parent_attempt_number,
 parent_actor.run_generation AS parent_actor_run_generation,
 wait.id AS run_wait_id,wait.suspend_checkpoint_id
FROM run_leases l JOIN runs r ON r.id=l.run_id AND r.computer_id=l.computer_id
 AND r.current_attempt_number=l.attempt_number AND r.current_run_lease_id=l.id
JOIN worker_hosts h ON h.id=l.worker_host_id AND h.worker_group_id=l.worker_group_id AND h.current_epoch=l.worker_epoch
JOIN worker_groups g ON g.id=l.worker_group_id AND g.region_id=l.region_id
JOIN computer_instances i ON i.id=l.computer_instance_id AND i.computer_id=l.computer_id
 AND i.writer_generation=l.writer_generation AND i.worker_host_id=l.worker_host_id AND i.worker_epoch=l.worker_epoch
LEFT JOIN sessions s ON s.id=r.session_id AND s.environment_id=r.environment_id
LEFT JOIN runs parent ON parent.id=r.parent_run_id AND parent.environment_id=r.environment_id
LEFT JOIN sessions parent_actor ON parent_actor.id=parent.session_id
LEFT JOIN run_waits wait ON wait.run_id=l.run_id AND wait.attempt_number=l.attempt_number
 AND wait.current_run_lease_id=l.id AND wait.suspension_status IN ('hot','checkpointing','resuming')
WHERE l.id=sqlc.arg(id) AND l.lease_sequence=sqlc.arg(lease_sequence)
 AND l.worker_group_id=sqlc.arg(worker_group_id) AND l.worker_host_id=sqlc.arg(worker_host_id)
 AND l.worker_epoch=sqlc.arg(worker_epoch) AND h.status IN ('active','draining') AND g.status IN ('active','draining')
 AND i.desired_state='ready' AND i.observed_state='ready' AND i.mount_state='mounted'
 AND i.writer_expires_at>clock_timestamp() AND i.reclaimed_at IS NULL
 AND l.expires_at>clock_timestamp() AND l.status IN ('starting','running') AND r.status IN ('queued','running','waiting') AND (l.status='running' OR l.start_deadline_at>clock_timestamp());

-- name: GetRunLeaseSecretDeliveryLocators :one
SELECT run_leases.environment_id,
       run_leases.run_id,
       run_leases.computer_id,
       run_leases.attempt_number
  FROM run_leases
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.status IN ('active', 'draining')
  JOIN worker_hosts
    ON worker_hosts.id = run_leases.worker_host_id
   AND worker_hosts.worker_group_id = run_leases.worker_group_id
   AND worker_hosts.current_epoch = run_leases.worker_epoch
   AND worker_hosts.status IN ('active', 'draining')
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_host_id = sqlc.arg(worker_host_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.status IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp();

-- name: GetLiveRunLeaseLocators :one
SELECT l.org_id,l.project_id,l.environment_id,l.run_id,l.computer_id,l.attempt_number,
 l.region_id,l.computer_instance_id,l.writer_generation,i.source_checkpoint_id,
 r.session_id,s.run_generation AS actor_run_generation,r.parent_run_id,r.parent_owns_lifecycle,
 parent.session_id AS parent_session_id,parent.current_attempt_number AS parent_attempt_number,
 parent_actor.run_generation AS parent_actor_run_generation,
 wait.id AS run_wait_id,wait.suspend_checkpoint_id
FROM run_leases l JOIN runs r ON r.id=l.run_id AND r.computer_id=l.computer_id
 AND r.current_attempt_number=l.attempt_number AND r.current_run_lease_id=l.id
JOIN worker_hosts h ON h.id=l.worker_host_id AND h.worker_group_id=l.worker_group_id AND h.current_epoch=l.worker_epoch
JOIN worker_groups g ON g.id=l.worker_group_id AND g.region_id=l.region_id
JOIN computer_instances i ON i.id=l.computer_instance_id AND i.computer_id=l.computer_id
 AND i.writer_generation=l.writer_generation AND i.worker_host_id=l.worker_host_id AND i.worker_epoch=l.worker_epoch
LEFT JOIN sessions s ON s.id=r.session_id AND s.environment_id=r.environment_id
LEFT JOIN runs parent ON parent.id=r.parent_run_id AND parent.environment_id=r.environment_id
LEFT JOIN sessions parent_actor ON parent_actor.id=parent.session_id
LEFT JOIN run_waits wait ON wait.run_id=l.run_id AND wait.attempt_number=l.attempt_number
 AND wait.current_run_lease_id=l.id AND wait.suspension_status IN ('hot','checkpointing','resuming')
WHERE l.id=sqlc.arg(id) AND l.lease_sequence=sqlc.arg(lease_sequence)
 AND l.worker_group_id=sqlc.arg(worker_group_id) AND l.worker_host_id=sqlc.arg(worker_host_id)
 AND l.worker_epoch=sqlc.arg(worker_epoch) AND h.status IN ('active','draining') AND g.status IN ('active','draining')
 AND i.desired_state='ready' AND i.observed_state='ready' AND i.mount_state='mounted'
 AND i.writer_expires_at>clock_timestamp() AND i.reclaimed_at IS NULL
 AND l.expires_at>clock_timestamp() AND l.status IN ('running','checkpointing','finalizing') AND r.status IN ('running','waiting');

-- name: LockRunLeaseClaimRun :one
SELECT *
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND computer_id = sqlc.arg(computer_id)
 FOR UPDATE;

-- name: LockRunFinalizationParentRun :one
SELECT *
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimActor :one
SELECT *
  FROM sessions
 WHERE id = sqlc.arg(id)
   AND computer_id = sqlc.arg(computer_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimComputer :one
SELECT computers.id,
       computers.environment_id,
       computers.region_id,
       computers.sandbox_declared_id,
       computers.computer_spec_id,
       computers.creation_deployment_id,
       computers.key,
       computers.revision,
       computers.writer_generation,
       computers.head_disk_version_id,
       computers.status,
       computers.desired_state,
       computers.dirty_state,
       computers.last_activity_at,
       computers.created_at,
       computers.updated_at,
       computers.deleted_at
  FROM computers
  JOIN environments ON environments.id = computers.environment_id
 WHERE computers.id = sqlc.arg(id)
   AND environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND computers.environment_id = sqlc.arg(environment_id)
   AND computers.region_id = sqlc.arg(region_id)
 FOR UPDATE OF computers;

-- name: LockRunLeaseClaimAttempt :one
SELECT *
  FROM run_attempts
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND computer_id = sqlc.arg(computer_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimWorkerGroup :one
SELECT *
  FROM worker_groups
 WHERE id = sqlc.arg(id)
   AND region_id = sqlc.arg(region_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimWorker :one
SELECT *
  FROM worker_hosts
 WHERE id = sqlc.arg(id)
   AND worker_group_id = sqlc.arg(worker_group_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimReadyWorker :one
SELECT sqlc.embed(worker_hosts),
       COALESCE((worker_hosts.observed_at >= transaction_timestamp()
            - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
        AND worker_hosts.run_paused_reason IS NULL), false)::boolean AS run_ready
  FROM worker_hosts
 WHERE id = sqlc.arg(id)
   AND worker_group_id = sqlc.arg(worker_group_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimLease :one
SELECT *
 FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status IN ('assigned', 'starting')
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: LockRunStartLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status IN ('starting', 'running')
   AND expires_at > transaction_timestamp()
   AND (status = 'running' OR start_deadline_at > transaction_timestamp())
 FOR UPDATE;

-- name: LockLiveRunLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status IN ('running', 'checkpointing', 'finalizing')
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: GetRunLeaseRenewalTime :one
SELECT clock_timestamp()::timestamptz;

-- name: RenewRunLeaseExpiry :one
UPDATE run_leases
   SET previous_expires_at = expires_at,
       renewed_at = sqlc.arg(renewed_at),
       expires_at = sqlc.arg(expires_at),
       updated_at = sqlc.arg(renewed_at)
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND expires_at = sqlc.arg(previous_expires_at)
   AND status IN ('running', 'checkpointing')
 RETURNING *;

-- name: LockRunLeaseClaimWait :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND computer_id = sqlc.arg(computer_id)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
 FOR UPDATE;

-- name: LockRunStartWait :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
 FOR UPDATE;

-- name: LockReadyComputerCheckpoint :one
SELECT c.* FROM computer_checkpoints c JOIN computer_checkpoint_runs m ON m.checkpoint_id=c.id
JOIN run_waits w ON w.id=m.run_wait_id AND w.suspend_checkpoint_id=c.id
WHERE c.id=sqlc.arg(id) AND c.computer_id=sqlc.arg(computer_id)
 AND m.run_id=sqlc.arg(run_id) AND m.attempt_number=sqlc.arg(attempt_number) AND m.run_wait_id=sqlc.arg(run_wait_id)
 AND c.status='ready' AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp()) FOR UPDATE OF c;

-- name: MarkRunLeaseStarting :one
UPDATE run_leases
   SET status = 'starting',
       claimed_at = clock_timestamp(),
       updated_at = clock_timestamp()
 WHERE id = sqlc.arg(id)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_host_id = sqlc.arg(worker_host_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND status = 'assigned'
   AND start_deadline_at > clock_timestamp()
   AND expires_at > clock_timestamp()
RETURNING *;

-- name: GetRunFinalizationTime :one
SELECT clock_timestamp()::timestamptz;

-- name: RunFinalizationScopeIsClear :one
SELECT NOT EXISTS(SELECT 1 FROM run_waits w WHERE w.run_id=sqlc.arg(run_id)
 AND w.attempt_number=sqlc.arg(attempt_number) AND w.computer_id=sqlc.arg(computer_id)
 AND w.suspension_status NOT IN ('released','cancelled','failed')) AS clear;

-- name: BeginRunLeaseFinalization :one
UPDATE run_leases
   SET status = 'finalizing',
       expires_at = sqlc.arg(expires_at),
       finalization_operation_id = sqlc.arg(finalization_operation_id),
       finalization_started_at = sqlc.arg(finalization_started_at),
       finalization_request_fingerprint = sqlc.arg(finalization_request_fingerprint),
       updated_at = sqlc.arg(finalization_started_at)
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status = 'running'
   AND expires_at = sqlc.arg(previous_expires_at)
   AND sqlc.arg(expires_at)::timestamptz > expires_at
   AND finalization_operation_id IS NULL
   AND finalization_started_at IS NULL
   AND finalization_request_fingerprint IS NULL
RETURNING *;

-- name: CloseRunActiveIntervalForFinalization :one
UPDATE runs
   SET active_elapsed_ms = active_elapsed_ms
           + floor(extract(epoch FROM (sqlc.arg(finalization_started_at)::timestamptz - active_started_at)) * 1000)::bigint,
       active_started_at = NULL,
       revision = revision + 1,
       updated_at = sqlc.arg(finalization_started_at)
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND computer_id = sqlc.arg(computer_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND revision = sqlc.arg(expected_revision)
   AND active_started_at IS NOT NULL
   AND sqlc.arg(finalization_started_at)::timestamptz >= active_started_at
   AND sqlc.arg(finalization_started_at)::timestamptz < active_started_at
       + ((max_active_duration_ms - active_elapsed_ms) * interval '1 millisecond')
RETURNING *;

-- name: MarkRunLeaseRunning :one
UPDATE run_leases
   SET status = 'running',
       started_at = clock_timestamp(),
       updated_at = clock_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_host_id = sqlc.arg(worker_host_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND computer_instance_id = sqlc.arg(computer_instance_id)
   AND status = 'starting'
   AND start_deadline_at > clock_timestamp()
   AND expires_at > clock_timestamp()
RETURNING *;

-- name: MarkRunRunning :one
UPDATE runs
   SET status = 'running',
       started_at = coalesce(started_at, clock_timestamp()),
       active_started_at = clock_timestamp(),
       revision = revision + 1,
       updated_at = clock_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND computer_id = sqlc.arg(computer_id)
   AND revision = sqlc.arg(expected_revision)
   AND status = 'queued'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: TouchRunComputerActivity :one
UPDATE computers
   SET last_activity_at = greatest(last_activity_at, clock_timestamp()),
       updated_at = clock_timestamp()
 WHERE computers.id = sqlc.arg(id)
   AND computers.environment_id = sqlc.arg(environment_id)
   AND EXISTS (
       SELECT 1 FROM environments
        WHERE environments.id = computers.environment_id
          AND environments.org_id = sqlc.arg(org_id)
          AND environments.project_id = sqlc.arg(project_id)
   )
   AND computers.writer_generation = sqlc.arg(writer_generation)
   AND computers.status = 'active'
   AND computers.desired_state = 'active'
RETURNING computers.id, computers.environment_id, computers.region_id, computers.sandbox_declared_id, computers.computer_spec_id, computers.creation_deployment_id, computers.key, computers.revision, computers.writer_generation, computers.head_disk_version_id, computers.status, computers.desired_state, computers.dirty_state, computers.last_activity_at, computers.created_at, computers.updated_at, computers.deleted_at;

-- name: MarkRunEntrypointEntered :one
UPDATE run_attempts
   SET entrypoint_entered_at = clock_timestamp()
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND computer_id = sqlc.arg(computer_id)
   AND entrypoint_entered_at IS NULL
   AND terminal_at IS NULL
RETURNING *;

-- Discovery only. Recovery locks Computer, instance and logical scopes before
-- deciding outcome. Expiry cannot prove process exclusion or reclaim an instance.
-- name: ListRunExecutionLeaseRecoveryCandidates :many
SELECT r.org_id,r.project_id,r.environment_id,r.id AS run_id,r.computer_id,
 r.current_attempt_number,l.id AS run_lease_id
FROM run_leases l JOIN runs r ON r.id=l.run_id AND r.current_run_lease_id=l.id
 AND r.current_attempt_number=l.attempt_number
JOIN computer_instances i ON i.id=l.computer_instance_id AND i.writer_generation=l.writer_generation
JOIN worker_hosts h ON h.id=l.worker_host_id
WHERE l.status IN ('assigned','starting','running','checkpointing','finalizing')
 AND (l.expires_at<=clock_timestamp()
 OR (l.status IN ('assigned','starting') AND l.start_deadline_at<=clock_timestamp())
 OR h.current_epoch<>l.worker_epoch OR h.status IN ('lost','termination_ready')
 OR i.observed_state IN ('failed','lost','closed') OR i.writer_expires_at<=clock_timestamp()
 OR (r.active_started_at IS NOT NULL AND r.active_started_at
     + GREATEST(r.max_active_duration_ms-r.active_elapsed_ms,0)*interval '1 millisecond'<=clock_timestamp()))
ORDER BY l.expires_at,l.id LIMIT sqlc.arg(limit_count);

-- name: LockRunLeaseClaimInstance :one
SELECT *
  FROM computer_instances
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND region_id = sqlc.arg(region_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_host_id = sqlc.arg(worker_host_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND computer_id = sqlc.arg(computer_id)
 FOR UPDATE;

-- Caller holds every execution authority lock; time is evaluated afterwards.
-- name: GetRunLeaseExecutionLive :one
SELECT (l.expires_at>clock_timestamp() AND (l.status NOT IN ('assigned','starting') OR l.start_deadline_at>clock_timestamp())
 AND i.writer_expires_at>clock_timestamp() AND (sqlc.arg(skip_worker_readiness)::boolean OR (h.observed_at>=clock_timestamp()-sqlc.arg(worker_freshness_seconds)::bigint*interval '1 second'
 AND h.run_paused_reason IS NULL)))::boolean AS live
 FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id
 JOIN worker_hosts h ON h.id=l.worker_host_id WHERE l.id=sqlc.arg(id);
