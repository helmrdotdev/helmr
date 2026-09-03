-- name: CreateControlOutbox :one
INSERT INTO control_outbox (
    id,
    topic,
    payload,
    available_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(topic),
    sqlc.arg(payload),
    sqlc.arg(available_at)
)
RETURNING *;

-- name: ClaimControlOutbox :many
WITH candidates AS (
    SELECT id
    FROM control_outbox
    WHERE (
        (state = 'pending' AND available_at <= now())
        OR
        (state = 'claimed' AND claim_expires_at <= now())
      )
      AND control_outbox.topic = ANY(sqlc.arg(topics)::text[])
    ORDER BY available_at, id
    LIMIT sqlc.arg(row_limit)
    FOR UPDATE SKIP LOCKED
)
UPDATE control_outbox
SET state = 'claimed',
    attempts = attempts + 1,
    claimed_by = sqlc.arg(claimed_by),
    claim_expires_at = sqlc.arg(claim_expires_at)
FROM candidates
WHERE control_outbox.id = candidates.id
RETURNING control_outbox.*;

-- name: DeliverControlOutbox :one
UPDATE control_outbox
SET state = 'delivered',
    claimed_by = NULL,
    claim_expires_at = NULL,
    last_error = NULL,
    delivered_at = now()
WHERE id = sqlc.arg(id)
  AND state = 'claimed'
  AND claimed_by = sqlc.arg(claimed_by)
  AND attempts = sqlc.arg(claim_attempt)
  AND claim_expires_at > now()
RETURNING *;

-- name: RetryControlOutbox :one
UPDATE control_outbox
SET state = 'pending',
    claimed_by = NULL,
    claim_expires_at = NULL,
    available_at = sqlc.arg(available_at),
    last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)
  AND state = 'claimed'
  AND claimed_by = sqlc.arg(claimed_by)
  AND attempts = sqlc.arg(claim_attempt)
  AND claim_expires_at > now()
RETURNING *;

-- name: DeadLetterControlOutbox :one
UPDATE control_outbox
SET state = 'dead_lettered',
    claimed_by = NULL,
    claim_expires_at = NULL,
    last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)
  AND state = 'claimed'
  AND claimed_by = sqlc.arg(claimed_by)
  AND attempts = sqlc.arg(claim_attempt)
  AND claim_expires_at > now()
RETURNING *;

-- name: DeadLetterUnsupportedControlOutbox :many
WITH candidates AS MATERIALIZED (
    SELECT id
    FROM control_outbox
    WHERE state = 'pending'
      AND NOT (topic = ANY(sqlc.arg(supported_topics)::text[]))
    ORDER BY created_at, id
    LIMIT sqlc.arg(row_limit)
)
UPDATE control_outbox
SET state = 'dead_lettered',
    last_error = 'unsupported control outbox topic'
FROM candidates
WHERE control_outbox.id = candidates.id
  AND control_outbox.state = 'pending'
RETURNING control_outbox.*;

-- name: PruneDeliveredControlOutbox :execrows
DELETE FROM control_outbox
 WHERE id IN (
    SELECT id
      FROM control_outbox
     WHERE state = 'delivered'
       AND delivered_at < now() - sqlc.arg(retain_for)::interval
     ORDER BY delivered_at, id
     LIMIT sqlc.arg(row_limit)
 );

-- name: ControlOutboxLifecycle :one
WITH dead_letter_sample AS (
    SELECT 1
      FROM control_outbox
     WHERE state = 'dead_lettered'
     ORDER BY created_at, id
     LIMIT (sqlc.arg(dead_letter_limit)::integer + 1)
),
dead_letter_counts AS (
    SELECT COUNT(*)::bigint AS rows
      FROM dead_letter_sample
)
SELECT (
           SELECT available_at
             FROM control_outbox
            WHERE state = 'pending'
              AND available_at <= now()
            ORDER BY available_at, id
            LIMIT 1
       )::timestamptz AS oldest_eligible_at,
       LEAST(dead_letter_counts.rows, sqlc.arg(dead_letter_limit)::bigint)::bigint AS dead_lettered_rows,
       (dead_letter_counts.rows > sqlc.arg(dead_letter_limit)::bigint)::boolean AS dead_lettered_overflow
  FROM dead_letter_counts;
