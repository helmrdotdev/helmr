-- name: LockIdempotencySlot :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: FindLiveIdempotencyClaims :many
SELECT idempotency_claims.*,
       coalesce(idempotency_claims.expires_at <= transaction_timestamp(), false)::boolean AS expired
  FROM idempotency_claims
  JOIN unnest(sqlc.arg(hash_key_versions)::integer[])
       WITH ORDINALITY AS versions(hash_key_version, position)
    ON versions.hash_key_version = idempotency_claims.hash_key_version
  JOIN unnest(sqlc.arg(scope_hashes)::bytea[])
       WITH ORDINALITY AS scopes(scope_hash, position)
    ON scopes.position = versions.position
   AND scopes.scope_hash = idempotency_claims.scope_hash
  JOIN unnest(sqlc.arg(key_hashes)::bytea[])
       WITH ORDINALITY AS keys(key_hash, position)
    ON keys.position = versions.position
   AND keys.key_hash = idempotency_claims.key_hash
 WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)
   AND idempotency_claims.operation = sqlc.arg(operation)
   AND idempotency_claims.retired_at IS NULL
 ORDER BY idempotency_claims.generation DESC, idempotency_claims.id
 LIMIT 2;

-- name: GetLatestIdempotencyClaimGeneration :one
SELECT coalesce(max(idempotency_claims.generation), 0)::bigint
  FROM idempotency_claims
  JOIN unnest(sqlc.arg(hash_key_versions)::integer[])
       WITH ORDINALITY AS versions(hash_key_version, position)
    ON versions.hash_key_version = idempotency_claims.hash_key_version
  JOIN unnest(sqlc.arg(scope_hashes)::bytea[])
       WITH ORDINALITY AS scopes(scope_hash, position)
    ON scopes.position = versions.position
   AND scopes.scope_hash = idempotency_claims.scope_hash
  JOIN unnest(sqlc.arg(key_hashes)::bytea[])
       WITH ORDINALITY AS keys(key_hash, position)
    ON keys.position = versions.position
   AND keys.key_hash = idempotency_claims.key_hash
 WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)
   AND idempotency_claims.operation = sqlc.arg(operation);

-- name: ListIdempotencyHashKeyUsage :many
SELECT hash_key_version, count(*)::bigint AS claim_count
  FROM idempotency_claims
 GROUP BY hash_key_version
 ORDER BY hash_key_version;

-- name: RetireExpiredIdempotencyClaims :many
WITH candidates AS (
    SELECT id
      FROM idempotency_claims
     WHERE retired_at IS NULL
       AND expires_at <= transaction_timestamp()
     ORDER BY expires_at, id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
UPDATE idempotency_claims
   SET retired_at = statement_timestamp()
  FROM candidates
 WHERE idempotency_claims.id = candidates.id
RETURNING idempotency_claims.*;

-- name: CollectRetiredIdempotencyClaims :many
WITH candidates AS (
    SELECT id
      FROM idempotency_claims
     WHERE retired_at IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
             FROM runs
            WHERE runs.claim_id = idempotency_claims.id
       )
       AND NOT EXISTS (
           SELECT 1
             FROM actor_records
            WHERE actor_records.claim_id = idempotency_claims.id
       )
       AND NOT EXISTS (
           SELECT 1
             FROM run_stream_records
            WHERE run_stream_records.claim_id = idempotency_claims.id
       )
       AND NOT EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.child_claim_id = idempotency_claims.id
       )
     ORDER BY retired_at, id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
DELETE FROM idempotency_claims
 USING candidates
 WHERE idempotency_claims.id = candidates.id
RETURNING idempotency_claims.*;

-- name: CreateIdempotencyClaim :one
INSERT INTO idempotency_claims (
    id,
    environment_id,
    operation,
    scope_hash,
    key_hash,
    hash_key_version,
    generation,
    request_fingerprint,
    accepted_at,
    expires_at
)
SELECT
    sqlc.arg(id),
    sqlc.arg(environment_id),
    sqlc.arg(operation),
    sqlc.arg(scope_hash),
    sqlc.arg(key_hash),
    sqlc.arg(hash_key_version),
    sqlc.arg(generation),
    sqlc.arg(request_fingerprint),
    statement_timestamp(),
    CASE
        WHEN sqlc.arg(operation)::text = 'task.child.invoke' THEN NULL
        ELSE statement_timestamp() + interval '30 days'
    END
  FROM lookup_hmac_versions
 WHERE version = sqlc.arg(hash_key_version)
   AND is_current
   AND retired_at IS NULL
RETURNING *;

-- name: GetIdempotencyClaim :one
SELECT *
  FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: CompleteIdempotencyClaim :one
UPDATE idempotency_claims
   SET state = 'completed',
       receipt = sqlc.arg(receipt),
       completed_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND request_fingerprint = sqlc.arg(request_fingerprint)
   AND state = 'pending'
   AND retired_at IS NULL
RETURNING *;

-- name: FailIdempotencyClaim :one
UPDATE idempotency_claims
   SET state = 'failed',
       receipt = sqlc.arg(receipt),
       completed_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND request_fingerprint = sqlc.arg(request_fingerprint)
   AND state = 'pending'
   AND retired_at IS NULL
RETURNING *;

-- name: RetireExpiredIdempotencyClaim :one
UPDATE idempotency_claims
   SET retired_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND retired_at IS NULL
   AND expires_at <= now()
RETURNING *;
