-- Register the immutable generation identity before its objects are uploaded.
-- Runtime object pins retain the candidate until publication or reclamation.
-- name: RegisterRunFinalizationObject :one
INSERT INTO run_finalization_objects (run_lease_id, operation_id, root, lease_status)
SELECT id, finalization_operation_id, sqlc.arg(root), status FROM run_leases
 WHERE id=sqlc.arg(run_lease_id) AND status='finalizing'
   AND finalization_operation_id=sqlc.arg(operation_id)
ON CONFLICT (run_lease_id) DO UPDATE SET run_lease_id=run_finalization_objects.run_lease_id
 WHERE run_finalization_objects.operation_id=EXCLUDED.operation_id
   AND run_finalization_objects.root=EXCLUDED.root
   AND run_finalization_objects.lease_status='finalizing'
RETURNING *;

-- name: RequireRunFinalizationObject :one
SELECT * FROM run_finalization_objects
 WHERE run_lease_id=sqlc.arg(run_lease_id) AND operation_id=sqlc.arg(operation_id)
   AND root=sqlc.arg(root) AND lease_status='finalizing';
