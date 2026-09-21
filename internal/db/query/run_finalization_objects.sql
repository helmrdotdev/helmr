-- The caller locks current finalization authority before registering immutable
-- bytes. The lease transition releases the availability pin atomically with
-- outcome publication, or makes an uncommitted upload eligible for reclamation.
-- name: RegisterRunFinalizationObject :one
WITH lifetime AS (
    INSERT INTO cas_object_lifetimes (digest) VALUES (sqlc.arg(digest))
    ON CONFLICT DO NOTHING
)
INSERT INTO run_finalization_objects (
    run_lease_id, operation_id, digest, size_bytes, media_type, logical_bytes, lease_status
)
SELECT id, finalization_operation_id, sqlc.arg(digest), sqlc.arg(size_bytes),
       sqlc.arg(media_type), sqlc.arg(logical_bytes), status
  FROM run_leases
 WHERE id=sqlc.arg(run_lease_id) AND status='finalizing'
   AND finalization_operation_id=sqlc.arg(operation_id)
ON CONFLICT (run_lease_id) DO UPDATE SET run_lease_id=run_finalization_objects.run_lease_id
 WHERE run_finalization_objects.operation_id=EXCLUDED.operation_id
   AND run_finalization_objects.digest=EXCLUDED.digest
   AND run_finalization_objects.size_bytes=EXCLUDED.size_bytes
   AND run_finalization_objects.media_type=EXCLUDED.media_type
   AND run_finalization_objects.logical_bytes=EXCLUDED.logical_bytes
   AND run_finalization_objects.lease_status='finalizing'
RETURNING *;

-- name: RequireRunFinalizationObject :one
SELECT * FROM run_finalization_objects
 WHERE run_lease_id=sqlc.arg(run_lease_id) AND operation_id=sqlc.arg(operation_id)
   AND digest=sqlc.arg(digest) AND size_bytes=sqlc.arg(size_bytes)
   AND media_type=sqlc.arg(media_type) AND logical_bytes=sqlc.arg(logical_bytes)
   AND lease_status='finalizing';
