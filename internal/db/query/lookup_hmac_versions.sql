-- name: LockActiveLookupHMACVersions :many
SELECT *
  FROM lookup_hmac_versions
 WHERE retired_at IS NULL
 ORDER BY version
 FOR SHARE;

-- name: ListLookupHMACVersions :many
SELECT *
  FROM lookup_hmac_versions
 ORDER BY version;

-- name: LockLookupHMACMaintenance :exec
SELECT pg_advisory_xact_lock(-6440469322911146185);

-- name: LockLookupHMACVersionsForMaintenance :many
SELECT *
  FROM lookup_hmac_versions
 ORDER BY version
 FOR UPDATE;

-- name: ClearCurrentLookupHMACVersion :execrows
UPDATE lookup_hmac_versions
   SET is_current = false
 WHERE is_current;

-- name: CreateCurrentLookupHMACVersion :one
INSERT INTO lookup_hmac_versions (
    version,
    key_fingerprint,
    is_current
)
VALUES (
    sqlc.arg(version),
    sqlc.arg(key_fingerprint),
    true
)
RETURNING *;

-- name: RetireLookupHMACVersion :one
UPDATE lookup_hmac_versions
   SET retired_at = statement_timestamp()
 WHERE lookup_hmac_versions.version = sqlc.arg(version)
   AND NOT is_current
   AND retired_at IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM idempotency_claims
        WHERE hash_key_version = sqlc.arg(version)
   )
   AND NOT EXISTS (
       SELECT 1
         FROM secret_versions
        WHERE authenticator_key_version = sqlc.arg(version)
   )
RETURNING *;
