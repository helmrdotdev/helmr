-- Only abandoned uploads are retirement candidates. Each lifetime belongs to one
-- organization's keyspace; its membership and registered owners pin
-- availability with FKs. These NOT EXISTS clauses avoid routine conflicts but
-- are not the concurrency barrier.
-- name: ListAbandonedCasBlobs :many
SELECT lifetime.org_id, lifetime.digest
  FROM cas_blobs lifetime
 WHERE lifetime.retired_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM boot_seed_objects c WHERE c.org_id=lifetime.org_id AND c.digest=lifetime.digest)
   AND NOT EXISTS (SELECT 1 FROM checkpoint_objects c WHERE c.org_id=lifetime.org_id AND c.digest=lifetime.digest)
   AND NOT EXISTS (SELECT 1 FROM disk_objects c WHERE c.org_id=lifetime.org_id AND c.digest=lifetime.digest)
   AND NOT EXISTS (SELECT 1 FROM cas_objects o WHERE o.org_id=lifetime.org_id AND o.digest=lifetime.digest)
   AND NOT EXISTS (SELECT 1 FROM allocation_upload_receipts w WHERE w.org_id=lifetime.org_id AND w.digest=lifetime.digest)
 ORDER BY lifetime.org_id, lifetime.digest
 LIMIT sqlc.arg(row_limit);

-- Commit before making any remote calls. Never clear retired_at or remove this
-- row: an in-flight upload can finish after a successful empty sweep.
-- name: RetireAbandonedCasBlob :execrows
UPDATE cas_blobs lifetime
   SET retired_at=clock_timestamp(), next_reclaim_at=clock_timestamp()
 WHERE lifetime.org_id=sqlc.arg(org_id) AND lifetime.digest=sqlc.arg(digest)
   AND lifetime.retired_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM allocation_upload_receipts w WHERE w.org_id=lifetime.org_id AND w.digest=lifetime.digest)
;

-- Upload receipts authorize nothing once their allocation ends. Removing them
-- lets an upload abandoned by that allocation become a retirement candidate.
-- name: DeleteEndedUploadReceipts :execrows
DELETE FROM allocation_upload_receipts w
 WHERE NOT EXISTS (
    SELECT 1 FROM computer_leases l WHERE l.worker_host_id=w.worker_host_id AND l.worker_epoch=w.worker_epoch AND l.allocation_id=w.allocation_id AND l.fenced_at IS NULL)
   AND NOT EXISTS (
    SELECT 1 FROM image_builds b WHERE b.worker_host_id=w.worker_host_id AND b.worker_epoch=w.worker_epoch AND b.allocation_id=w.allocation_id AND b.fenced_at IS NULL)
   AND NOT EXISTS (
    SELECT 1 FROM boot_seed_builds b WHERE b.worker_host_id=w.worker_host_id AND b.worker_epoch=w.worker_epoch AND b.allocation_id=w.allocation_id AND b.fenced_at IS NULL);

-- Claim only scheduling responsibility, not permission to adopt the digest.
-- A crashed sweeper becomes eligible again; repeated deletion is idempotent.
-- name: ClaimRetiredCasBlobs :many
WITH due AS (
    SELECT org_id, digest FROM cas_blobs
     WHERE retired_at IS NOT NULL AND next_reclaim_at <= statement_timestamp()
     ORDER BY next_reclaim_at, org_id, digest LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
UPDATE cas_blobs lifetime
   SET next_reclaim_at=clock_timestamp() + interval '5 minutes'
  FROM due WHERE lifetime.org_id=due.org_id AND lifetime.digest=due.digest
RETURNING lifetime.org_id, lifetime.digest;

-- name: RecordCasBlobReclamation :exec
UPDATE cas_blobs
   SET next_reclaim_at=clock_timestamp() + interval '5 minutes',
       last_reclaim_error=sqlc.narg(last_error)
 WHERE org_id=sqlc.arg(org_id) AND digest=sqlc.arg(digest) AND retired_at IS NOT NULL;

-- name: RegisterRetiredCasUpload :exec
INSERT INTO cas_upload_reclaims (org_id, digest, upload_id)
SELECT lifetime.org_id, lifetime.digest, sqlc.arg(upload_id) FROM cas_blobs lifetime
 WHERE lifetime.org_id=sqlc.arg(org_id) AND lifetime.digest=sqlc.arg(digest) AND lifetime.retired_at IS NOT NULL
ON CONFLICT DO NOTHING;

-- Retained upload IDs get independent, fair retry scheduling. Historical IDs
-- must not monopolize the digest's deadline or starve completed-version cleanup.
-- name: ClaimRetiredCasUploads :many
WITH due AS (
    SELECT pending.org_id, pending.digest, pending.upload_id FROM cas_upload_reclaims pending
     WHERE pending.org_id=sqlc.arg(org_id) AND pending.digest=sqlc.arg(digest) AND pending.next_reclaim_at <= statement_timestamp()
     ORDER BY pending.next_reclaim_at, pending.upload_id LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
UPDATE cas_upload_reclaims upload
   SET next_reclaim_at=clock_timestamp() + interval '5 minutes'
  FROM due WHERE upload.org_id=due.org_id AND upload.digest=due.digest AND upload.upload_id=due.upload_id
RETURNING upload.upload_id;
