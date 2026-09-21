-- Only abandoned uploads are retirement candidates. Memberships in any org and
-- registered owners pin availability with FKs; these NOT EXISTS clauses avoid
-- routine conflicts but are not the concurrency barrier.
-- name: ListAbandonedCasObjects :many
SELECT lifetime.digest
  FROM cas_object_lifetimes lifetime
 WHERE lifetime.retired_at IS NULL
   AND (EXISTS (SELECT 1 FROM computer_initializations i WHERE i.digest=lifetime.digest AND i.status='abandoned')
        OR EXISTS (SELECT 1 FROM run_checkpoint_objects c WHERE c.digest=lifetime.digest AND c.checkpoint_status IN ('invalid','deleted'))
        OR EXISTS (SELECT 1 FROM run_finalization_objects f WHERE f.digest=lifetime.digest AND f.lease_status IN ('completed','failed','cancelled','lost','expired')))
   AND NOT EXISTS (SELECT 1 FROM cas_objects o WHERE o.digest=lifetime.digest)
 ORDER BY lifetime.digest
 LIMIT sqlc.arg(row_limit);

-- Commit before making any remote calls. Never clear retired_at or remove this
-- row: an in-flight upload can finish after a successful empty sweep.
-- name: RetireAbandonedCasObject :execrows
UPDATE cas_object_lifetimes lifetime
   SET retired_at=clock_timestamp(), next_reclaim_at=clock_timestamp()
 WHERE lifetime.digest=sqlc.arg(digest)
   AND lifetime.retired_at IS NULL
   AND (EXISTS (SELECT 1 FROM computer_initializations i WHERE i.digest=lifetime.digest AND i.status='abandoned')
        OR EXISTS (SELECT 1 FROM run_checkpoint_objects c WHERE c.digest=lifetime.digest AND c.checkpoint_status IN ('invalid','deleted'))
        OR EXISTS (SELECT 1 FROM run_finalization_objects f WHERE f.digest=lifetime.digest AND f.lease_status IN ('completed','failed','cancelled','lost','expired')));

-- Claim only scheduling responsibility, not permission to adopt the digest.
-- A crashed sweeper becomes eligible again; repeated deletion is idempotent.
-- name: ClaimRetiredCasObjects :many
WITH due AS (
    SELECT digest FROM cas_object_lifetimes
     WHERE retired_at IS NOT NULL AND next_reclaim_at <= statement_timestamp()
     ORDER BY next_reclaim_at, digest LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
UPDATE cas_object_lifetimes lifetime
   SET next_reclaim_at=clock_timestamp() + interval '5 minutes'
  FROM due WHERE lifetime.digest=due.digest
RETURNING lifetime.digest;

-- name: RecordCasReclamation :exec
UPDATE cas_object_lifetimes
   SET next_reclaim_at=clock_timestamp() + interval '5 minutes',
       last_reclaim_error=sqlc.narg(last_error)
 WHERE digest=sqlc.arg(digest) AND retired_at IS NOT NULL;

-- name: RegisterRetiredCasUpload :exec
INSERT INTO cas_retired_uploads (digest, upload_id)
SELECT lifetime.digest, sqlc.arg(upload_id) FROM cas_object_lifetimes lifetime
 WHERE lifetime.digest=sqlc.arg(digest) AND lifetime.retired_at IS NOT NULL
ON CONFLICT DO NOTHING;

-- name: ListRetiredCasUploads :many
SELECT upload_id FROM cas_retired_uploads WHERE digest=sqlc.arg(digest) ORDER BY upload_id;

-- Retained upload IDs get independent, fair retry scheduling. Historical IDs
-- must not monopolize the digest's deadline or starve completed-version cleanup.
-- name: ClaimRetiredCasUploads :many
WITH due AS (
    SELECT pending.digest, pending.upload_id FROM cas_retired_uploads pending
     WHERE pending.digest=sqlc.arg(digest) AND pending.next_reclaim_at <= statement_timestamp()
     ORDER BY pending.next_reclaim_at, pending.upload_id LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
UPDATE cas_retired_uploads upload
   SET next_reclaim_at=clock_timestamp() + interval '5 minutes'
  FROM due WHERE upload.digest=due.digest AND upload.upload_id=due.upload_id
RETURNING upload.upload_id;
