-- Register the immutable generation identity before its objects are uploaded.
-- The lease owns candidate identity and retains it as terminal retry evidence.
-- name: RegisterRunFinalizationRoot :one
UPDATE run_leases SET finalization_root=sqlc.arg(root)
 WHERE id=sqlc.arg(run_lease_id) AND status='finalizing'
   AND finalization_operation_id=sqlc.arg(operation_id)
   AND (finalization_root IS NULL OR finalization_root=sqlc.arg(root))
RETURNING id;

-- name: RequireRunFinalizationRoot :one
SELECT id FROM run_leases
 WHERE id=sqlc.arg(run_lease_id) AND finalization_operation_id=sqlc.arg(operation_id)
   AND finalization_root=sqlc.arg(root) AND status='finalizing';
