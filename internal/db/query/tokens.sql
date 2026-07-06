-- name: CreateToken :one
WITH existing_token AS MATERIALIZED (
    SELECT tokens.*
     FROM tokens
     WHERE tokens.org_id = sqlc.arg(org_id)
       AND tokens.project_id = sqlc.arg(project_id)
       AND tokens.environment_id = sqlc.arg(environment_id)
       AND tokens.idempotency_key = sqlc.arg(idempotency_key)
       AND sqlc.arg(idempotency_key)::text <> ''
     FOR UPDATE
),
inserted_token AS (
    INSERT INTO tokens (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        timeout_at,
        idempotency_key,
        idempotency_key_expires_at,
        create_request_fingerprint,
        callback_key_id,
        callback_secret_fingerprint,
        callback_secret_created_at,
        metadata,
        tags
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           sqlc.arg(org_id),
           sqlc.arg(project_id),
           sqlc.arg(environment_id),
           sqlc.arg(timeout_at),
           COALESCE(sqlc.arg(idempotency_key)::text, ''),
           sqlc.narg(idempotency_key_expires_at)::timestamptz,
           COALESCE(sqlc.arg(create_request_fingerprint)::text, ''),
           COALESCE(sqlc.arg(callback_key_id)::text, ''),
           COALESCE(sqlc.arg(callback_secret_fingerprint)::text, ''),
           sqlc.narg(callback_secret_created_at)::timestamptz,
           COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
           COALESCE(sqlc.arg(tags)::text[], '{}'::text[])
     WHERE NOT EXISTS (SELECT 1 FROM existing_token)
    RETURNING tokens.*
),
selected_token AS (
    SELECT inserted_token.*, false::boolean AS is_cached
      FROM inserted_token
    UNION ALL
    SELECT existing_token.*, true::boolean AS is_cached
      FROM existing_token
)
SELECT selected_token.*,
       (
           selected_token.is_cached
           AND selected_token.create_request_fingerprint <> COALESCE(sqlc.arg(create_request_fingerprint)::text, '')
       )::boolean AS idempotency_fingerprint_mismatch
  FROM selected_token;

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
 WHERE id = sqlc.arg(id)
   AND callback_key_id = sqlc.arg(callback_key_id)
   AND callback_secret_fingerprint = sqlc.arg(callback_secret_fingerprint)
   AND state IN ('pending', 'completed')
 FOR UPDATE;

-- name: CompleteToken :one
WITH target AS (
    SELECT tokens.*
     FROM tokens
     WHERE tokens.org_id = sqlc.arg(org_id)
       AND tokens.project_id = sqlc.arg(project_id)
       AND tokens.environment_id = sqlc.arg(environment_id)
       AND tokens.id = sqlc.arg(id)
       AND tokens.state IN ('pending', 'completed')
     FOR UPDATE
),
completed AS (
    UPDATE tokens
       SET state = 'completed',
           completion_data = COALESCE(sqlc.arg(completion_data)::jsonb, 'null'::jsonb),
           completion_content_type = COALESCE(NULLIF(sqlc.arg(completion_content_type)::text, ''), 'application/json'),
           completion_fingerprint = COALESCE(sqlc.arg(completion_fingerprint)::text, ''),
           completed_at = now(),
           updated_at = now()
     FROM target
     WHERE tokens.org_id = target.org_id
       AND tokens.id = target.id
       AND target.state = 'pending'
       AND target.timeout_at > now()
    RETURNING tokens.*
),
selected_token AS (
    SELECT completed.*, false::boolean AS was_already_completed, false::boolean AS is_expired
      FROM completed
    UNION ALL
    SELECT target.*,
           (target.state = 'completed')::boolean AS was_already_completed,
           (target.state = 'pending' AND target.timeout_at <= now())::boolean AS is_expired
      FROM target
     WHERE NOT EXISTS (SELECT 1 FROM completed)
),
matched_wait AS (
    UPDATE waits
       SET state = 'completed',
           result = COALESCE(selected_token.completion_data, 'null'::jsonb),
           completed_at = COALESCE(selected_token.completed_at, now()),
           updated_at = now()
      FROM selected_token
     WHERE waits.org_id = selected_token.org_id
       AND waits.project_id = selected_token.project_id
       AND waits.environment_id = selected_token.environment_id
       AND waits.token_id = selected_token.id
       AND waits.kind = 'token'
       AND waits.state = 'pending'
       AND selected_token.state = 'completed'
    RETURNING waits.id, waits.org_id
),
resolved_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
     FROM matched_wait
     WHERE run_waits.org_id = matched_wait.org_id
       AND run_waits.wait_id = matched_wait.id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.id
)
SELECT selected_token.*,
       (
           selected_token.was_already_completed
           AND selected_token.completion_fingerprint = COALESCE(sqlc.arg(completion_fingerprint)::text, '')
       )::boolean AS already_completed,
       (
           selected_token.was_already_completed
           AND selected_token.completion_fingerprint <> COALESCE(sqlc.arg(completion_fingerprint)::text, '')
       )::boolean AS completion_conflict,
       selected_token.is_expired AS completion_expired,
       (SELECT count(*) FROM resolved_wait)::bigint AS resolved_wait_count
  FROM selected_token;

-- name: CancelToken :one
WITH cancelled AS (
    UPDATE tokens
       SET state = 'cancelled',
           cancelled_at = now(),
           updated_at = now()
     WHERE tokens.org_id = sqlc.arg(org_id)
       AND tokens.project_id = sqlc.arg(project_id)
       AND tokens.environment_id = sqlc.arg(environment_id)
       AND tokens.id = sqlc.arg(id)
       AND tokens.state = 'pending'
       AND tokens.timeout_at > now()
    RETURNING tokens.*
),
matched_wait AS (
    UPDATE waits
       SET state = 'cancelled',
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
     FROM cancelled
     WHERE waits.org_id = cancelled.org_id
       AND waits.project_id = cancelled.project_id
       AND waits.environment_id = cancelled.environment_id
       AND waits.token_id = cancelled.id
       AND waits.kind = 'token'
       AND waits.state = 'pending'
    RETURNING waits.id, waits.org_id
),
resolved_cancelled_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
     FROM matched_wait
     WHERE run_waits.org_id = matched_wait.org_id
       AND run_waits.wait_id = matched_wait.id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.id
)
SELECT cancelled.*, (SELECT count(*) FROM resolved_cancelled_wait)::bigint AS resolved_wait_count
  FROM cancelled;

-- name: ExpireDueTokens :many
WITH expired AS (
    UPDATE tokens
       SET state = 'expired',
           expired_at = now(),
           updated_at = now()
     WHERE tokens.org_id = sqlc.arg(org_id)
       AND tokens.state = 'pending'
       AND tokens.timeout_at <= now()
    RETURNING tokens.*
),
matched_wait AS (
    UPDATE waits
       SET state = 'expired',
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM expired
     WHERE waits.org_id = expired.org_id
       AND waits.project_id = expired.project_id
       AND waits.environment_id = expired.environment_id
       AND waits.token_id = expired.id
       AND waits.kind = 'token'
       AND waits.state = 'pending'
    RETURNING waits.id, waits.org_id
),
expired_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM matched_wait
     WHERE run_waits.org_id = matched_wait.org_id
       AND run_waits.wait_id = matched_wait.id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.id
)
SELECT *
  FROM expired
 ORDER BY expired.timeout_at ASC, expired.id ASC;
