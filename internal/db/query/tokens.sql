-- name: GetTokenCreateTime :one
SELECT transaction_timestamp()::timestamptz;

-- name: CreateToken :one
INSERT INTO tokens (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    expires_at,
    callback_key_id,
    callback_secret_fingerprint,
    metadata,
    tags
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(expires_at)::timestamptz,
    sqlc.arg(callback_key_id),
    sqlc.arg(callback_secret_fingerprint),
    COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    COALESCE(sqlc.arg(tags)::text[], '{}'::text[])
)
RETURNING *;

-- name: GetToken :one
SELECT *
 FROM tokens
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetTokenByID :one
SELECT *
  FROM tokens
 WHERE id = sqlc.arg(id);

-- name: GetTokenByPublicID :one
SELECT *
  FROM tokens
 WHERE public_id = sqlc.arg(public_id);

-- name: ListTokens :many
WITH cursor_token AS (
    SELECT created_at, id
     FROM tokens
     WHERE org_id = sqlc.arg(org_id)
       AND project_id = sqlc.arg(project_id)
       AND environment_id = sqlc.arg(environment_id)
       AND id = sqlc.narg(after_id)::uuid
)
SELECT *
 FROM tokens
 WHERE tokens.org_id = sqlc.arg(org_id)
   AND tokens.project_id = sqlc.arg(project_id)
   AND tokens.environment_id = sqlc.arg(environment_id)
   AND (
       sqlc.narg(state)::text IS NULL
       OR tokens.state = sqlc.narg(state)::token_state
   )
   AND (
       sqlc.narg(after_id)::uuid IS NULL
       OR (tokens.created_at, tokens.id) > (SELECT cursor_token.created_at, cursor_token.id FROM cursor_token)
   )
 ORDER BY tokens.created_at ASC, tokens.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: GetTokenForCallbackCompletion :one
SELECT *
 FROM tokens
 WHERE public_id = sqlc.arg(public_id)
   AND callback_secret_fingerprint = sqlc.arg(callback_secret_fingerprint)
 FOR UPDATE;

-- name: CompleteToken :one
WITH target AS MATERIALIZED (
    SELECT tokens.*
     FROM tokens
     WHERE tokens.org_id = sqlc.arg(org_id)
       AND tokens.project_id = sqlc.arg(project_id)
       AND tokens.environment_id = sqlc.arg(environment_id)
       AND tokens.id = sqlc.arg(id)
     FOR UPDATE
),
expired AS (
    UPDATE tokens
       SET state = 'expired',
           expired_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
     FROM target
     WHERE tokens.id = target.id
       AND target.state = 'pending'
       AND target.expires_at <= transaction_timestamp()
    RETURNING tokens.*
),
completed AS (
    UPDATE tokens
       SET state = 'completed',
           result = COALESCE(sqlc.arg(result)::jsonb, 'null'::jsonb),
           error = NULL,
           completion_fingerprint = sqlc.arg(completion_fingerprint),
           completed_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
     FROM target
     WHERE tokens.id = target.id
       AND target.state = 'pending'
       AND target.expires_at > transaction_timestamp()
    RETURNING tokens.*
),
selected_token AS (
    SELECT completed.* FROM completed
    UNION ALL
    SELECT expired.* FROM expired
    UNION ALL
    SELECT target.*
      FROM target
     WHERE NOT EXISTS (SELECT 1 FROM completed)
       AND NOT EXISTS (SELECT 1 FROM expired)
),
changed AS (
    SELECT id, environment_id FROM completed
    UNION ALL
    SELECT id, environment_id FROM expired
),
reconciliation_intent AS (
    INSERT INTO outbox_messages (
        lane,
        topic,
        partition_key,
        payload,
        available_at
    )
    SELECT 'control',
           'token.reconcile',
           changed.id::text,
           jsonb_build_object(
               'environmentId', changed.environment_id::text,
               'tokenId', changed.id::text
           ),
           transaction_timestamp()
      FROM changed
    RETURNING id
)
SELECT selected_token.*,
       (
           selected_token.state = 'completed'
           AND selected_token.completion_fingerprint = sqlc.arg(completion_fingerprint)::bytea
           AND NOT EXISTS (SELECT 1 FROM completed)
       )::boolean AS already_completed,
       (
           selected_token.state = 'completed'
           AND selected_token.completion_fingerprint <> sqlc.arg(completion_fingerprint)::bytea
           AND NOT EXISTS (SELECT 1 FROM completed)
       )::boolean AS completion_conflict,
       (selected_token.state = 'expired')::boolean AS completion_expired,
       (selected_token.state = 'cancelled')::boolean AS completion_cancelled,
       EXISTS (SELECT 1 FROM reconciliation_intent) AS reconciliation_enqueued
  FROM selected_token;

-- name: CancelToken :one
WITH target AS MATERIALIZED (
    SELECT tokens.*
     FROM tokens
     WHERE tokens.org_id = sqlc.arg(org_id)
       AND tokens.project_id = sqlc.arg(project_id)
       AND tokens.environment_id = sqlc.arg(environment_id)
       AND tokens.id = sqlc.arg(id)
     FOR UPDATE
),
expired AS (
    UPDATE tokens
       SET state = 'expired',
           expired_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
     FROM target
     WHERE tokens.id = target.id
       AND target.state = 'pending'
       AND target.expires_at <= transaction_timestamp()
    RETURNING tokens.*
),
cancelled AS (
    UPDATE tokens
       SET state = 'cancelled',
           cancelled_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
     FROM target
     WHERE tokens.id = target.id
       AND target.state = 'pending'
       AND target.expires_at > transaction_timestamp()
    RETURNING tokens.*
),
selected_token AS (
    SELECT cancelled.* FROM cancelled
    UNION ALL
    SELECT expired.* FROM expired
    UNION ALL
    SELECT target.*
      FROM target
     WHERE NOT EXISTS (SELECT 1 FROM cancelled)
       AND NOT EXISTS (SELECT 1 FROM expired)
),
changed AS (
    SELECT id, environment_id FROM cancelled
    UNION ALL
    SELECT id, environment_id FROM expired
),
reconciliation_intent AS (
    INSERT INTO outbox_messages (
        lane,
        topic,
        partition_key,
        payload,
        available_at
    )
    SELECT 'control',
           'token.reconcile',
           changed.id::text,
           jsonb_build_object(
               'environmentId', changed.environment_id::text,
               'tokenId', changed.id::text
           ),
           transaction_timestamp()
      FROM changed
    RETURNING id
)
SELECT selected_token.*,
       (
           selected_token.state = 'cancelled'
           AND NOT EXISTS (SELECT 1 FROM cancelled)
       )::boolean AS already_cancelled,
       (selected_token.state = 'expired')::boolean AS cancellation_expired,
       (selected_token.state = 'completed')::boolean AS cancellation_completed,
       EXISTS (SELECT 1 FROM reconciliation_intent) AS reconciliation_enqueued
  FROM selected_token;

-- name: ExpireDueTokens :many
WITH candidates AS MATERIALIZED (
    SELECT id
     FROM tokens
     WHERE state = 'pending'
       AND expires_at <= transaction_timestamp()
     ORDER BY expires_at, id
     FOR UPDATE SKIP LOCKED
     LIMIT sqlc.arg(limit_count)
),
expired AS (
    UPDATE tokens
       SET state = 'expired',
           expired_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
     FROM candidates
     WHERE tokens.id = candidates.id
       AND tokens.state = 'pending'
    RETURNING tokens.*
),
reconciliation_intents AS (
    INSERT INTO outbox_messages (
        lane,
        topic,
        partition_key,
        payload,
        available_at
    )
    SELECT 'control',
           'token.reconcile',
           expired.id::text,
           jsonb_build_object(
               'environmentId', expired.environment_id::text,
               'tokenId', expired.id::text
           ),
           transaction_timestamp()
      FROM expired
    RETURNING partition_key
)
SELECT expired.*
  FROM expired
  JOIN reconciliation_intents
    ON reconciliation_intents.partition_key = expired.id::text
 ORDER BY expired.expires_at, expired.id;
