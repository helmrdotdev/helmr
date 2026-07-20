-- name: AcquireIdempotencyClaim :one
INSERT INTO idempotency_claims (
    id,
    environment_id,
    operation,
    scope_hash,
    key_hash,
    hash_key_version,
    generation,
    request_fingerprint,
    expires_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(environment_id),
    sqlc.arg(operation),
    sqlc.arg(scope_hash),
    sqlc.arg(key_hash),
    sqlc.arg(hash_key_version),
    sqlc.arg(generation),
    sqlc.arg(request_fingerprint),
    sqlc.arg(expires_at)
)
ON CONFLICT (environment_id, operation, scope_hash, key_hash)
    WHERE retired_at IS NULL
DO UPDATE SET operation = excluded.operation
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
