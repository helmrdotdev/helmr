-- name: ListComputerMembers :many
WITH run_states AS (
    SELECT runs.id,
           CASE
             WHEN runs.status IN ('succeeded','failed','cancelled','expired','system_failed')
               AND EXISTS(SELECT 1 FROM run_leases l WHERE l.run_id=runs.id AND l.process_reconciled_at IS NULL)
               THEN 'unreconciled'
             WHEN runs.status = 'cancel_requested' THEN 'draining'
             WHEN runs.status IN ('waiting','queued') AND EXISTS (
                 SELECT 1 FROM run_waits
                  WHERE run_waits.run_id = runs.id
                    AND run_waits.attempt_number = runs.current_attempt_number
                    AND run_waits.suspension_status IN ('parked','resume_pending')
             ) THEN 'parked'
             WHEN runs.status = 'waiting' THEN 'waiting'
             WHEN runs.status = 'running' THEN 'running'
             ELSE 'admitted'
           END::text AS state
      FROM runs
     WHERE runs.environment_id = sqlc.arg(environment_id)
       AND runs.computer_id = sqlc.arg(computer_id)
), members AS (
    SELECT 'session'::text AS kind, sessions.id, sessions.current_run_id AS run_id,
           CASE WHEN sessions.status IN ('closed','failed') THEN 'unreconciled'
                WHEN sessions.status = 'closing' THEN 'draining'
                ELSE coalesce(run_states.state, 'admitted') END::text AS state,
           sessions.created_at
      FROM sessions
      LEFT JOIN run_states ON run_states.id = sessions.current_run_id
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.computer_id = sqlc.arg(computer_id)
       AND (sessions.status IN ('open', 'closing') OR EXISTS(
           SELECT 1 FROM runs r JOIN run_leases l ON l.run_id=r.id
            WHERE r.session_id=sessions.id AND l.process_reconciled_at IS NULL))
    UNION ALL
    SELECT 'task'::text, runs.id, runs.id, run_states.state, runs.created_at
      FROM runs
      JOIN run_states ON run_states.id = runs.id
     WHERE runs.environment_id = sqlc.arg(environment_id)
       AND runs.computer_id = sqlc.arg(computer_id)
       AND runs.entrypoint_kind = 'task'
       AND (runs.status IN ('queued','running','waiting','retry_delayed','cancel_requested')
            OR EXISTS(SELECT 1 FROM run_leases l WHERE l.run_id=runs.id AND l.process_reconciled_at IS NULL))
    UNION ALL
    SELECT 'command'::text, command.id, NULL::uuid,
           CASE WHEN command.terminal_at IS NOT NULL THEN 'unreconciled'
                WHEN command.status = 'stopping' THEN 'draining'
                WHEN command.status = 'running' THEN 'running'
                ELSE 'admitted' END::text,
           command.created_at
      FROM computer_commands AS command
     WHERE command.environment_id = sqlc.arg(environment_id)
       AND command.computer_id = sqlc.arg(computer_id)
       AND (command.terminal_at IS NULL
            OR (command.computer_instance_id IS NOT NULL AND command.process_reconciled_at IS NULL))
)
SELECT kind, id, run_id, state, created_at FROM members
 WHERE NOT sqlc.arg(has_after)::boolean
    OR (created_at, id, kind) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid, sqlc.arg(after_kind)::text)
 ORDER BY created_at DESC, id DESC, kind DESC
 LIMIT sqlc.arg(row_limit);
