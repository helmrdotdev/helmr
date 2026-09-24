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

-- name: LockActorCloseWorkspace :one
SELECT id, environment_id, region_id, sandbox_declared_id, deployment_definition_id, key, revision, owner_session_id, owner_run_id, ownership_generation, writer_generation, head_version_id, status, desired_state, dirty_state, last_activity_at, created_at, updated_at, deleted_at
  FROM computers
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(workspace_id)
   AND owner_session_id = sqlc.arg(session_id)
   AND owner_run_id IS NULL
 FOR UPDATE;

-- name: GetActorCloseWorkspaceActivity :one
SELECT EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.workspace_id = sqlc.arg(workspace_id)
              AND workspace_leases.status IN ('active', 'releasing')
       ) AS has_active_lease,
       EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = sqlc.arg(workspace_id)
              AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
       ) AS has_active_process,
       EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.workspace_id = sqlc.arg(workspace_id)
              AND run_waits.condition_status = 'pending'
              AND run_waits.child_run_id IS NOT NULL
       ) AS has_active_child;

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
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'closing'
   AND current_run_id IS NULL
   AND active_turn_id IS NULL AND dispatch_hold_id IS NULL
   AND close_sequence IS NOT NULL
   AND committed_input_sequence >= close_sequence
RETURNING *;

-- name: CreateActorCloseReconcileOutbox :exec
INSERT INTO control_outbox (id, topic, payload, available_at)
VALUES (
    sqlc.arg(id),
    'session.close.reconcile',
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
