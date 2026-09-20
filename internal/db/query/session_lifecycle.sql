-- name: EnqueueSessionTurn :one
WITH allocated AS (
 UPDATE sessions SET next_input_sequence=next_input_sequence+1,revision=revision+1,updated_at=now()
 WHERE sessions.environment_id=sqlc.arg(environment_id) AND sessions.id=sqlc.arg(session_id)
   AND status='open' AND next_input_sequence <= 9007199254740991
 RETURNING *,next_input_sequence-1 AS sequence
)
INSERT INTO session_turns(id,environment_id,session_id,sequence,data,source_run_id)
SELECT sqlc.arg(id),allocated.environment_id,allocated.id,allocated.sequence,sqlc.arg(data),sqlc.narg(source_run_id)
FROM allocated RETURNING *;

-- name: GetSessionTurn :one
SELECT * FROM session_turns WHERE environment_id=$1 AND session_id=$2 AND id=$3;

-- name: ListSessionEvents :many
SELECT e.*,r.deployment_id FROM session_events e
LEFT JOIN runs r ON r.id=e.producer_run_id
WHERE e.environment_id=sqlc.arg(environment_id) AND e.session_id=sqlc.arg(session_id) AND e.sequence > sqlc.arg(after_sequence)
ORDER BY e.sequence LIMIT sqlc.arg(limit_count);

-- name: SessionTurnAcceptsMessages :one
SELECT EXISTS (
 SELECT 1 FROM session_turns t
 JOIN sessions s ON s.id=t.session_id AND s.active_turn_id=t.id
   AND s.current_run_id=t.run_id AND s.run_generation=t.run_generation
 JOIN runs r ON r.id=t.run_id AND r.current_attempt_number=t.attempt_number
 WHERE t.environment_id=$1 AND t.session_id=$2 AND t.id=$3
   AND s.status IN ('open','closing') AND s.cancel_requested_at IS NULL AND s.dispatch_hold_id IS NULL
   AND t.status='running' AND t.interrupt_requested_at IS NULL AND t.settlement_started_at IS NULL
   AND r.status IN ('running','waiting','queued')
)::boolean AS accepts;

-- name: SessionTurnMessageReady :one
SELECT EXISTS (
 SELECT 1 FROM session_turns t
 JOIN sessions s ON s.id=t.session_id AND s.active_turn_id=t.id AND s.current_run_id=t.run_id AND s.run_generation=t.run_generation
 JOIN runs r ON r.id=t.run_id AND r.current_attempt_number=t.attempt_number
 JOIN run_leases l ON l.id=t.ready_run_lease_id AND l.id=r.current_run_lease_id AND l.run_id=r.id AND l.attempt_number=t.attempt_number
 JOIN worker_instances w ON w.id=l.worker_instance_id AND w.current_epoch=l.worker_epoch
 JOIN runtime_instances ri ON ri.id=l.runtime_instance_id
 WHERE t.environment_id=$1 AND t.session_id=$2 AND t.id=$3
   AND s.status IN ('open','closing') AND s.cancel_requested_at IS NULL AND s.dispatch_hold_id IS NULL
   AND t.status='running' AND t.interrupt_requested_at IS NULL AND t.settlement_started_at IS NULL
   AND r.status='running' AND l.status='running' AND l.expires_at > now()
   AND w.lost_at IS NULL AND w.termination_ready_at IS NULL
   AND ri.observed_state='ready' AND ri.reclaimed_at IS NULL
)::boolean AS ready;

-- name: SetSessionTurnMessageReady :one
UPDATE session_turns SET ready_run_lease_id=sqlc.arg(run_lease_id)
WHERE environment_id=sqlc.arg(environment_id) AND session_id=sqlc.arg(session_id) AND id=sqlc.arg(turn_id)
  AND status='running' AND settlement_started_at IS NULL AND interrupt_requested_at IS NULL
RETURNING *;

-- name: CreateSessionMessage :one
INSERT INTO session_messages(id,environment_id,session_id,turn_id,run_id,attempt_number,run_generation,data,accepted_sequence)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING *;

-- name: LockSessionMessage :one
SELECT * FROM session_messages WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR UPDATE;

-- name: ClaimSessionMessage :one
UPDATE session_messages SET status='handling',delivery_id=sqlc.arg(delivery_id),
 delivery_run_lease_id=sqlc.arg(run_lease_id),handling_at=now()
WHERE session_messages.id=(SELECT candidate.id FROM session_messages candidate WHERE candidate.environment_id=sqlc.arg(environment_id)
 AND candidate.session_id=sqlc.arg(session_id) AND candidate.turn_id=sqlc.arg(turn_id) AND candidate.status='accepted'
 AND NOT EXISTS(SELECT 1 FROM session_messages m WHERE m.session_id=sqlc.arg(session_id) AND m.turn_id=sqlc.arg(turn_id) AND m.status='handling')
 ORDER BY candidate.accepted_sequence LIMIT 1 FOR UPDATE)
RETURNING *;

-- name: GetSessionMessageDelivery :one
SELECT * FROM session_messages WHERE environment_id=$1 AND session_id=$2 AND delivery_id=$3;

-- name: FinishSessionMessage :one
UPDATE session_messages SET status=sqlc.arg(status),outcome=sqlc.arg(outcome),terminal_at=now()
WHERE environment_id=sqlc.arg(environment_id) AND session_id=sqlc.arg(session_id) AND id=sqlc.arg(id)
 AND status=sqlc.arg(expected_status) RETURNING *;

-- name: ListUnsettledSessionMessages :many
SELECT * FROM session_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3
 AND status IN ('accepted','handling') ORDER BY accepted_sequence FOR UPDATE;

-- name: BeginSessionTurnSettlement :one
UPDATE session_turns SET settlement_started_at=coalesce(settlement_started_at,now()),ready_run_lease_id=NULL
WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND status='running'
 AND interrupt_requested_at IS NULL RETURNING *;

-- name: SessionTurnHasUnsettledWork :one
SELECT EXISTS(SELECT 1 FROM session_messages m WHERE m.session_id=sqlc.arg(session_id) AND m.turn_id=sqlc.arg(turn_id) AND m.status IN ('accepted','handling','unknown'))
 OR EXISTS(SELECT 1 FROM run_waits WHERE run_waits.turn_session_id=sqlc.arg(session_id) AND run_waits.turn_id=sqlc.arg(turn_id)
  AND (suspension_status NOT IN ('released','cancelled','failed')
   OR (child_run_id IS NOT NULL AND EXISTS(SELECT 1 FROM runs c WHERE c.id=run_waits.child_run_id AND c.status NOT IN ('succeeded','failed','cancelled','expired','system_failed'))))) AS unsettled;

-- name: HoldSessionExecution :one
UPDATE sessions SET dispatch_hold_id=sqlc.arg(hold_id),dispatch_hold_reason=sqlc.arg(reason),
 dispatch_hold_run_id=sqlc.arg(run_id),dispatch_hold_attempt_number=sqlc.arg(attempt_number),
 dispatch_hold_run_generation=run_generation,revision=revision+1,updated_at=now()
WHERE sessions.environment_id=sqlc.arg(environment_id) AND sessions.id=sqlc.arg(session_id)
 AND current_run_id=sqlc.arg(run_id) AND status IN ('open','closing') RETURNING *;

-- name: ClearSessionDispatchHold :one
UPDATE sessions SET dispatch_hold_id=NULL,dispatch_hold_reason=NULL,dispatch_hold_run_id=NULL,
 dispatch_hold_attempt_number=NULL,dispatch_hold_run_generation=NULL,revision=revision+1,updated_at=now()
WHERE environment_id=$1 AND id=$2 AND dispatch_hold_id=$3 AND active_turn_id IS NULL
 AND current_run_id IS NULL AND dispatch_hold_reason IN ('interrupted','recovered') RETURNING *;

-- name: SessionWriterExcluded :one
SELECT NOT EXISTS(SELECT 1 FROM workspace_leases WHERE workspace_leases.workspace_id=sqlc.arg(workspace_id) AND status IN ('active','releasing'))
 AND NOT EXISTS(SELECT 1 FROM workspace_processes WHERE workspace_processes.workspace_id=sqlc.arg(workspace_id) AND status IN ('pending','starting','running','exit_requested'))
 AND NOT EXISTS(SELECT 1 FROM runtime_instances WHERE runtime_instances.workspace_id=sqlc.arg(workspace_id) AND reclaimed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM run_waits w JOIN runs c ON c.id=w.child_run_id WHERE w.workspace_id=sqlc.arg(workspace_id)
  AND c.parent_owns_lifecycle AND c.status NOT IN ('succeeded','failed','cancelled','expired','system_failed')) AS excluded;

-- name: CompleteSessionRecovery :one
UPDATE sessions SET active_turn_id=NULL,current_run_id=NULL,dispatch_hold_id=sqlc.arg(new_hold_id),
 dispatch_hold_reason='recovered',revision=revision+1,updated_at=now(),
 committed_input_sequence=coalesce(sqlc.narg(input_sequence),committed_input_sequence)
WHERE sessions.environment_id=sqlc.arg(environment_id) AND sessions.id=sqlc.arg(session_id)
 AND dispatch_hold_id=sqlc.arg(hold_id) AND active_turn_id IS NOT DISTINCT FROM sqlc.narg(turn_id)
RETURNING *;

-- name: SettleHeldSessionTurn :one
UPDATE session_turns SET status=sqlc.arg(status),ready_run_lease_id=NULL,
 terminal_event_id=sqlc.arg(event_id),terminal_request_fingerprint=sqlc.arg(fingerprint)
WHERE environment_id=sqlc.arg(environment_id) AND session_id=sqlc.arg(session_id) AND id=sqlc.arg(turn_id)
 AND status='running' RETURNING *;

-- name: CompleteSessionInterruption :one
UPDATE sessions SET active_turn_id=NULL,current_run_id=NULL,
 dispatch_hold_id=sqlc.arg(new_hold_id),dispatch_hold_reason='interrupted',
 committed_input_sequence=coalesce(sqlc.narg(input_sequence),committed_input_sequence),
 revision=revision+1,updated_at=now()
WHERE environment_id=sqlc.arg(environment_id) AND id=sqlc.arg(session_id)
 AND current_run_id=sqlc.arg(run_id) AND run_generation=sqlc.arg(run_generation)
 AND dispatch_hold_id=sqlc.arg(hold_id) AND dispatch_hold_reason='interrupt_requested'
 AND active_turn_id IS NOT DISTINCT FROM sqlc.narg(turn_id)
RETURNING *;

-- name: BindRunWaitTurn :one
WITH bound AS (
 UPDATE run_waits SET turn_session_id=sqlc.arg(session_id),turn_id=sqlc.arg(turn_id),turn_run_generation=sqlc.arg(run_generation)
 WHERE run_waits.id=sqlc.arg(wait_id) AND run_waits.turn_id IS NULL
   AND EXISTS(SELECT 1 FROM sessions s JOIN session_turns t ON t.session_id=s.id AND t.id=s.active_turn_id
     WHERE s.id=sqlc.arg(session_id) AND s.active_turn_id=sqlc.arg(turn_id)
       AND s.current_run_id=run_waits.run_id AND s.run_generation=sqlc.arg(run_generation)
       AND s.dispatch_hold_id IS NULL AND t.status='running' AND t.attempt_number=run_waits.attempt_number
       AND t.settlement_started_at IS NULL AND t.interrupt_requested_at IS NULL)
 RETURNING run_waits.*
), unreadied AS (
 UPDATE session_turns SET ready_run_lease_id=NULL FROM bound
 WHERE session_turns.id=bound.turn_id RETURNING session_turns.id
)
SELECT bound.* FROM bound JOIN unreadied ON unreadied.id=bound.turn_id;

-- name: RunWaitTurnCurrent :one
SELECT ((r.session_id IS NULL OR EXISTS(SELECT 1 FROM sessions a WHERE a.id=r.session_id AND a.current_run_id=r.id AND a.dispatch_hold_id IS NULL)) AND
 (w.turn_id IS NULL OR EXISTS(SELECT 1 FROM sessions s JOIN session_turns t ON t.id=s.active_turn_id
 WHERE s.id=w.turn_session_id AND s.active_turn_id=w.turn_id AND s.current_run_id=w.run_id
 AND s.run_generation=w.turn_run_generation AND s.dispatch_hold_id IS NULL
 AND t.attempt_number=w.attempt_number AND t.status='running' AND t.interrupt_requested_at IS NULL)))::boolean AS current
FROM run_waits w JOIN runs r ON r.id=w.run_id WHERE w.id=$1;

-- name: LockWorkerSessionOperationActors :many
SELECT s.* FROM sessions s
WHERE s.environment_id=sqlc.arg(environment_id)
 AND (s.id=sqlc.arg(target_session_id) OR s.id=(SELECT w.owner_session_id FROM workspaces w WHERE w.id=sqlc.arg(source_workspace_id)))
ORDER BY s.id FOR UPDATE OF s;

-- name: SessionRecoveryHeadCommitted :one
SELECT EXISTS(SELECT 1 FROM workspace_versions
 WHERE environment_id=$1 AND workspace_id=$2 AND id=$3 AND status='committed') AS committed;

-- name: RunWaitSessionStopped :one
SELECT EXISTS (
 SELECT 1 FROM run_waits w JOIN runs r ON r.id=w.run_id
 JOIN sessions s ON s.id=r.session_id
 WHERE w.id=$1 AND s.current_run_id=r.id
 AND s.dispatch_hold_reason='interrupt_requested' AND s.dispatch_hold_run_id=r.id
 AND s.dispatch_hold_attempt_number=w.attempt_number AND s.dispatch_hold_run_generation=s.run_generation
 AND r.current_attempt_number=w.attempt_number
 AND (w.turn_id IS NULL OR (s.active_turn_id=w.turn_id AND w.turn_session_id=s.id AND w.turn_run_generation=s.run_generation))
) AS stopped;

-- name: SessionOwnedExecutionsExcluded :one
WITH RECURSIVE owned(id) AS (
 SELECT c.id FROM runs c WHERE c.parent_run_id=$1 AND c.parent_owns_lifecycle
 UNION
 SELECT c.id FROM owned p JOIN runs c ON c.parent_run_id=p.id WHERE c.parent_owns_lifecycle
), runtimes(id) AS (
 -- A terminal worker finalization receipt already proves this program quiesced.
 -- Its Workspace runtime may remain warm; only unproved execution needs reclaim.
 SELECT l.runtime_instance_id FROM run_leases l JOIN owned o ON o.id=l.run_id
 WHERE NOT (l.status IN ('completed','failed') AND l.terminal_request_fingerprint IS NOT NULL
   AND l.finalization_operation_id IS NOT NULL AND l.terminal_at IS NOT NULL)
 UNION
 SELECT rt.id FROM runtime_instances rt JOIN owned o ON o.id=rt.reserved_run_id
)
SELECT (NOT EXISTS(SELECT 1 FROM runs r JOIN owned o ON o.id=r.id
 WHERE r.current_run_lease_id IS NOT NULL OR r.status NOT IN ('succeeded','failed','cancelled','expired','system_failed'))
 AND NOT EXISTS(SELECT 1 FROM runtime_instances rt JOIN runtimes ON runtimes.id=rt.id
 WHERE rt.reclaimed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM workspace_leases wl JOIN run_leases l ON l.id=wl.owner_run_lease_id JOIN owned o ON o.id=l.run_id
 WHERE wl.status IN ('active','releasing'))
 AND NOT EXISTS(SELECT 1 FROM workspace_mounts m JOIN runtimes rt ON rt.id=m.runtime_instance_id
 WHERE m.status IN ('mounting','mounted','unmounting')))::boolean AS excluded;

-- name: ReadWorkerSessionControl :one
SELECT s.dispatch_hold_id, s.dispatch_hold_reason, s.active_turn_id
FROM run_leases l
JOIN runs r ON r.id=l.run_id AND r.current_run_lease_id=l.id
 AND r.current_attempt_number=l.attempt_number AND r.workspace_id=l.workspace_id
JOIN sessions s ON s.id=r.session_id AND s.current_run_id=r.id
JOIN run_attempts a ON a.run_id=r.id AND a.number=l.attempt_number
JOIN worker_instances wi ON wi.id=l.worker_instance_id AND wi.worker_group_id=l.worker_group_id AND wi.current_epoch=l.worker_epoch
JOIN worker_groups wg ON wg.id=l.worker_group_id
JOIN workspace_leases wl ON wl.owner_run_lease_id=l.id AND wl.workspace_id=l.workspace_id
JOIN runtime_instances rt ON rt.id=l.runtime_instance_id AND rt.runtime_identity_id=l.runtime_identity_id
WHERE l.id=sqlc.arg(run_lease_id) AND l.lease_sequence=sqlc.arg(lease_sequence)
 AND l.worker_group_id=sqlc.arg(worker_group_id) AND l.worker_instance_id=sqlc.arg(worker_instance_id) AND l.worker_epoch=sqlc.arg(worker_epoch)
 AND l.status IN ('running','checkpointing') AND l.expires_at>statement_timestamp()
 AND l.finalization_operation_id IS NULL AND r.status IN ('running','waiting')
 AND a.entrypoint_entered_at IS NOT NULL AND a.terminal_at IS NULL
 AND s.run_generation=sqlc.arg(run_generation) AND s.status IN ('open','closing')
 AND wi.status IN ('active','draining') AND wg.status IN ('active','draining')
 AND wl.status='active' AND wl.expires_at>statement_timestamp()
 AND rt.observed_state='ready' AND rt.reclaimed_at IS NULL;
