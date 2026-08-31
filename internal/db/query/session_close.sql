-- name: LockActorClose :one
SELECT *
  FROM sessions
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
 FOR UPDATE;

-- name: BeginActorClose :one
UPDATE sessions
   SET state = CASE WHEN state = 'open' THEN 'closing' ELSE state END,
       close_sequence = CASE
           WHEN state = 'open' THEN next_input_sequence - 1
           ELSE close_sequence
       END,
       manual_run_cancelled = false,
       state_version = state_version + CASE
           WHEN state = 'open' OR manual_run_cancelled THEN 1
           ELSE 0
       END,
       updated_at = CASE
           WHEN state = 'open' OR manual_run_cancelled THEN transaction_timestamp()
           ELSE updated_at
       END
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
   AND state IN ('open', 'closing')
RETURNING *;

-- name: LockActorCloseWorkspace :one
SELECT *
  FROM workspaces
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
              AND workspace_leases.state IN ('active', 'releasing')
       ) AS has_active_lease,
       EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = sqlc.arg(workspace_id)
              AND workspace_processes.state IN ('pending', 'starting', 'running', 'exit_requested')
       ) AS has_active_process,
       EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.workspace_id = sqlc.arg(workspace_id)
              AND run_waits.condition_state = 'pending'
              AND run_waits.child_run_id IS NOT NULL
       ) AS has_active_child;

-- name: CompleteIdleActorClose :one
UPDATE sessions
   SET state = 'closed',
       current_run_id = NULL,
       run_generation = run_generation + 1,
       state_version = state_version + 1,
       closed_at = sqlc.arg(closed_at),
       updated_at = sqlc.arg(closed_at)
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'closing'
   AND current_run_id IS NULL
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
