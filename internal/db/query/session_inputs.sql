-- name: GetSessionTurnAtSequenceForUpdate :one
SELECT *
  FROM session_turns
 WHERE environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
   AND sequence = sqlc.arg(sequence)
 FOR UPDATE;

-- name: GetSessionTurnByIDForUpdate :one
SELECT *
  FROM session_turns
 WHERE environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
   AND id = sqlc.arg(id)
 FOR UPDATE;

-- name: CreateActorInputReconcileOutbox :exec
INSERT INTO control_outbox (id, topic, payload, available_at)
VALUES (
    sqlc.arg(id),
    'session.input.reconcile',
    jsonb_build_object(
        'environmentId', sqlc.arg(environment_id)::uuid::text,
        'sessionId', sqlc.arg(session_id)::uuid::text,
        'recordId', sqlc.arg(record_id)::uuid::text
    ),
    transaction_timestamp()
)
ON CONFLICT (id) DO NOTHING;

-- name: LockActorForInputReconcile :one
SELECT *
  FROM sessions
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
 FOR UPDATE;

-- name: LockActorInputCurrentRun :one
SELECT runs.*
  FROM runs
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.session_id = sqlc.arg(session_id)
 FOR UPDATE;
