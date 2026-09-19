-- name: LockSessionTurnAuthority :one
SELECT * FROM sessions WHERE environment_id = $1 AND id = $2 FOR UPDATE;

-- name: LockSessionTurnInput :one
SELECT * FROM session_records
 WHERE environment_id = $1 AND session_id = $2 AND id = $3 AND direction = 'input'
 FOR UPDATE;

-- name: ActivateSessionTurn :one
WITH activated AS (
    UPDATE sessions SET active_turn_id = sqlc.arg(turn_id),
           revision = revision + 1, updated_at = now()
     WHERE sessions.environment_id = sqlc.arg(environment_id) AND sessions.id = sqlc.arg(session_id)
       AND current_run_id = sqlc.arg(run_id) AND active_turn_id IS NULL
       AND dispatch_hold_id IS NULL AND status IN ('open', 'closing')
       AND committed_input_sequence + 1 = sqlc.arg(input_sequence)
       AND EXISTS (SELECT 1 FROM runs WHERE runs.id = sessions.current_run_id
                   AND current_attempt_number = sqlc.arg(attempt_number) AND status IN ('running', 'waiting'))
    RETURNING *
)
UPDATE session_records SET turn_status = 'running', run_generation = activated.run_generation,
       turn_run_id = sqlc.arg(run_id), turn_attempt_number = sqlc.arg(attempt_number)
  FROM activated WHERE session_records.session_id = activated.id
   AND session_records.id = activated.active_turn_id AND session_records.turn_status = 'queued'
RETURNING session_records.*;

-- name: AcceptSessionTurnInterrupt :one
WITH held AS (
    UPDATE sessions SET dispatch_hold_id = sqlc.arg(hold_id), dispatch_hold_reason = 'interrupt_requested',
           revision = revision + 1, updated_at = now()
     WHERE sessions.environment_id = sqlc.arg(environment_id) AND sessions.id = sqlc.arg(session_id)
       AND active_turn_id = sqlc.arg(turn_id) AND sessions.run_generation = sqlc.arg(run_generation)
       AND dispatch_hold_id IS NULL
    RETURNING *
)
UPDATE session_records SET interrupt_requested_at = now()
 FROM held WHERE session_records.session_id = held.id AND session_records.id = held.active_turn_id
 AND session_records.turn_status = 'running' AND interrupt_requested_at IS NULL
RETURNING session_records.*;

-- name: AppendSessionEvent :one
WITH allocated AS (
    UPDATE sessions SET next_event_sequence = next_event_sequence + 1
     WHERE sessions.environment_id = sqlc.arg(environment_id) AND sessions.id = sqlc.arg(session_id)
       AND next_event_sequence <= 9007199254740991
    RETURNING sessions.id, sessions.workspace_id, sessions.next_event_sequence - 1 AS sequence
)
INSERT INTO session_events (id, environment_id, session_id, workspace_id, turn_id, sequence, kind, data,
 producer_run_id, producer_attempt_number, run_generation, workspace_version_id)
SELECT sqlc.arg(id), sqlc.arg(environment_id), allocated.id, allocated.workspace_id, sqlc.arg(turn_id), allocated.sequence,
 sqlc.arg(kind), sqlc.arg(data), sqlc.arg(producer_run_id), sqlc.arg(producer_attempt_number),
 sqlc.arg(run_generation), sqlc.narg(workspace_version_id) FROM allocated
RETURNING *;

-- name: GetSessionEvent :one
SELECT * FROM session_events WHERE environment_id = $1 AND session_id = $2 AND id = $3;

-- name: SettleSessionTurn :one
WITH advanced AS (
 UPDATE sessions SET committed_input_sequence = sqlc.arg(input_sequence), active_turn_id = NULL,
        revision = revision + 1, updated_at = now()
 WHERE sessions.environment_id = sqlc.arg(environment_id) AND sessions.id = sqlc.arg(session_id)
   AND active_turn_id = sqlc.arg(turn_id) AND sessions.run_generation = sqlc.arg(run_generation)
   AND committed_input_sequence + 1 = sqlc.arg(input_sequence) AND dispatch_hold_id IS NULL
 RETURNING *
)
UPDATE session_records SET turn_status = sqlc.arg(status), terminal_event_id = sqlc.arg(event_id),
 terminal_request_fingerprint = sqlc.arg(fingerprint)
 FROM advanced WHERE session_records.session_id = advanced.id AND session_records.id = sqlc.arg(turn_id)
 AND session_records.turn_status = 'running' AND interrupt_requested_at IS NULL
RETURNING session_records.*;
