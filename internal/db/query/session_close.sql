-- name: LockActorClose :one
SELECT *
  FROM sessions
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
 FOR UPDATE;

-- name: BeginActorClose :one
UPDATE sessions
   SET status = CASE WHEN status = 'open' THEN 'closing' ELSE status END,
       close_sequence = CASE
           WHEN status = 'open' THEN next_input_sequence - 1
           ELSE close_sequence
       END,
       revision = revision + CASE
           WHEN status = 'open' THEN 1
           ELSE 0
       END,
       updated_at = CASE
           WHEN status = 'open' THEN transaction_timestamp()
           ELSE updated_at
       END
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
   AND status IN ('open', 'closing')
RETURNING *;

-- name: LockActorCloseComputer :one
SELECT c.* FROM computers c
WHERE c.environment_id=sqlc.arg(environment_id) AND c.id=sqlc.arg(computer_id)
 AND EXISTS(SELECT 1 FROM sessions s WHERE s.id=sqlc.arg(session_id)
 AND s.environment_id=c.environment_id AND s.computer_id=c.id)
FOR UPDATE OF c;

-- name: GetActorCloseComputerActivity :one
WITH RECURSIVE owned(id) AS (
 SELECT r.id FROM runs r WHERE r.session_id=sqlc.arg(session_id)
 UNION
 SELECT r.id FROM runs r JOIN owned p ON p.id=r.parent_run_id WHERE r.parent_owns_lifecycle
)
SELECT EXISTS(SELECT 1 FROM run_leases l JOIN owned o ON o.id=l.run_id
 WHERE l.process_reconciled_at IS NULL) AS has_active_lease,
 EXISTS(SELECT 1 FROM runs r JOIN owned o ON o.id=r.id
 WHERE r.session_id IS DISTINCT FROM sqlc.arg(session_id)
 AND r.status NOT IN ('succeeded','failed','cancelled','expired','system_failed')) AS has_active_child;

-- name: CompleteIdleActorClose :one
UPDATE sessions
   SET status = 'closed',
       current_run_id = NULL,
       run_generation = run_generation + 1,
       revision = revision + 1,
       closed_at = sqlc.arg(closed_at),
       updated_at = sqlc.arg(closed_at)
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
   AND computer_id = sqlc.arg(computer_id)
   AND status = 'closing'
   AND current_run_id IS NULL
   AND active_turn_id IS NULL AND dispatch_hold_id IS NULL
   AND close_sequence IS NOT NULL
   AND committed_input_sequence >= close_sequence
RETURNING *;

-- name: CreateSessionLifecycleReconcileOutbox :exec
INSERT INTO control_outbox (id, topic, payload, available_at)
VALUES (
    sqlc.arg(id),
    'session.lifecycle.reconcile',
    jsonb_build_object(
        'environmentId', sqlc.arg(environment_id)::uuid::text,
        'sessionId', sqlc.arg(session_id)::uuid::text
    ),
    transaction_timestamp()
)
ON CONFLICT (id) DO NOTHING;

-- name: BeginSessionCancellation :one
UPDATE sessions SET status='closing',close_sequence=coalesce(close_sequence,next_input_sequence-1),
 cancel_requested_at=coalesce(cancel_requested_at,now()),revision=revision+1,updated_at=now()
WHERE environment_id=$1 AND id=$2 AND status IN ('open','closing') RETURNING *;

-- name: LockQueuedSessionTurns :many
SELECT * FROM session_turns WHERE environment_id=$1 AND session_id=$2 AND status='queued'
ORDER BY sequence FOR UPDATE;

-- name: CancelQueuedSessionTurn :one
UPDATE session_turns SET status='cancelled',terminal_event_id=sqlc.arg(event_id)
WHERE environment_id=sqlc.arg(environment_id) AND session_id=sqlc.arg(session_id)
 AND id=sqlc.arg(turn_id) AND status='queued' RETURNING *;

-- name: AdvanceCancelledSessionInputs :one
UPDATE sessions s SET committed_input_sequence=close_sequence,revision=revision+1,updated_at=now()
WHERE s.environment_id=$1 AND s.id=$2 AND s.cancel_requested_at IS NOT NULL
 AND s.active_turn_id IS NULL AND s.current_run_id IS NULL
 AND NOT EXISTS (SELECT 1 FROM session_turns t WHERE t.session_id=s.id
   AND t.sequence>s.committed_input_sequence AND t.sequence<=s.close_sequence AND t.status<>'cancelled')
RETURNING *;
