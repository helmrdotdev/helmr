-- The caller owns the fenced publication transaction. These storage operations
-- neither authenticate a Worker nor accept an unverified ciphertext declaration.
-- Admission/edge/key mutation must lock the parent and reject certified objects;
-- descriptors and immediate dependencies are immutable after certification.

-- name: LockComputerObject :one
SELECT * FROM computer_objects
 WHERE environment_id=sqlc.arg(environment_id) AND computer_id=sqlc.arg(computer_id)
   AND digest=sqlc.arg(digest)
 FOR UPDATE;

-- The data-modifying CTE always runs in the same statement. Certification does
-- not depend on its inserted row count: every inherited key may already be direct.
-- name: CertifyComputerObject :execrows
WITH object AS MATERIALIZED (
 SELECT o.* FROM computer_objects o
 WHERE o.environment_id=sqlc.arg(environment_id) AND o.computer_id=sqlc.arg(computer_id)
   AND o.digest=sqlc.arg(digest) AND NOT o.certified
   AND EXISTS(SELECT 1 FROM computer_object_keys k WHERE k.environment_id=o.environment_id AND k.computer_id=o.computer_id AND k.digest=o.digest AND k.is_direct)
 FOR UPDATE
), summary AS (
 INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct)
 SELECT DISTINCT o.environment_id,o.computer_id,o.digest,k.key_id,false
 FROM object o
 JOIN computer_object_edges e ON e.environment_id=o.environment_id
   AND e.computer_id=o.computer_id AND e.parent_digest=o.digest
 JOIN computer_object_keys k ON k.environment_id=e.environment_id
   AND k.computer_id=e.computer_id AND k.digest=e.child_digest
 ON CONFLICT (environment_id,computer_id,digest,key_id) DO NOTHING
 RETURNING key_id
)
UPDATE computer_objects o SET certified_at=clock_timestamp()
 FROM object selected
 WHERE o.environment_id=selected.environment_id AND o.computer_id=selected.computer_id
   AND o.digest=selected.digest;

-- The root comes from the exact retained owner, never an arbitrary HTTP key list.
-- name: ListComputerObjectReadKeys :many
SELECT k.* FROM computer_object_keys r
 JOIN computer_objects o USING(environment_id,computer_id,digest)
 JOIN computer_data_keys k ON k.environment_id=r.environment_id AND k.computer_id=r.computer_id AND k.id=r.key_id
 WHERE r.environment_id=sqlc.arg(environment_id) AND r.computer_id=sqlc.arg(computer_id)
   AND r.digest=sqlc.arg(digest) AND o.certified AND k.available
 ORDER BY k.id;

-- name: HasRegisteredInitialComputerObject :one
SELECT EXISTS (
 SELECT 1 FROM runtime_instances r
 JOIN runtime_computer_object_pins p ON p.runtime_instance_id=r.id AND p.publication_key=sqlc.arg(publication_key) AND p.runtime_desired_version=r.desired_version
 JOIN computer_objects o ON o.environment_id=p.environment_id AND o.computer_id=p.computer_id AND o.digest=p.digest
 WHERE r.id=sqlc.arg(runtime_id) AND r.worker_instance_id=sqlc.arg(worker_id)
   AND r.worker_group_id=sqlc.arg(worker_group_id) AND r.worker_epoch=sqlc.arg(worker_epoch)
   AND r.desired_version=sqlc.arg(desired_version) AND r.reclaimed_at IS NULL
   AND o.digest=sqlc.arg(digest) AND o.inspection=sqlc.arg(inspection)::jsonb
);

-- Physical reclamation is monotonic and already requires exclusion evidence.
-- Bound cleanup independently of remote storage; graph/object FKs remain intact.
-- name: ReleaseReclaimedComputerObjects :execrows
WITH released AS (
 SELECT p.runtime_instance_id,p.publication_key,p.digest FROM runtime_computer_object_pins p
 JOIN runtime_instances r ON r.id=p.runtime_instance_id
 WHERE r.reclaimed_at IS NOT NULL
 ORDER BY p.runtime_instance_id,p.publication_key,p.digest LIMIT sqlc.arg(row_limit)
 FOR UPDATE OF p SKIP LOCKED
)
DELETE FROM runtime_computer_object_pins p USING released r
 WHERE p.runtime_instance_id=r.runtime_instance_id AND p.publication_key=r.publication_key AND p.digest=r.digest;

-- Roots, active publications and parent objects are the availability owners.
-- Version history is not deleted. The final DELETE's FKs arbitrate concurrent
-- adoption after this discovery snapshot.
-- name: ListUnreferencedComputerObjects :many
SELECT o.environment_id,o.computer_id,o.digest,o.org_id
 FROM computer_objects o
 WHERE NOT EXISTS (SELECT 1 FROM computer_version_roots r WHERE r.environment_id=o.environment_id AND r.computer_id=o.computer_id AND r.root_digest=o.digest)
 AND NOT EXISTS (SELECT 1 FROM runtime_computer_object_pins p WHERE p.environment_id=o.environment_id AND p.computer_id=o.computer_id AND p.digest=o.digest)
 AND NOT EXISTS (SELECT 1 FROM computer_object_edges e WHERE e.environment_id=o.environment_id AND e.computer_id=o.computer_id AND e.child_digest=o.digest)
 ORDER BY o.rank DESC,o.environment_id,o.computer_id,o.digest
 LIMIT sqlc.arg(row_limit);

-- name: DeleteUnreferencedComputerObject :execrows
DELETE FROM computer_objects o
 WHERE o.environment_id=sqlc.arg(environment_id) AND o.computer_id=sqlc.arg(computer_id) AND o.digest=sqlc.arg(digest)
 AND NOT EXISTS (SELECT 1 FROM computer_version_roots r WHERE r.environment_id=o.environment_id AND r.computer_id=o.computer_id AND r.root_digest=o.digest)
 AND NOT EXISTS (SELECT 1 FROM runtime_computer_object_pins p WHERE p.environment_id=o.environment_id AND p.computer_id=o.computer_id AND p.digest=o.digest)
 AND NOT EXISTS (SELECT 1 FROM computer_object_edges e WHERE e.environment_id=o.environment_id AND e.computer_id=o.computer_id AND e.child_digest=o.digest);

-- Run only after deleting the selected Computer object in the same transaction.
-- Other Computers and artifact kinds may still own the shared physical bytes.
-- name: DeleteUnreferencedComputerCasMembership :execrows
DELETE FROM cas_objects c
 WHERE c.org_id=sqlc.arg(org_id) AND c.digest=sqlc.arg(digest)
 AND NOT EXISTS (SELECT 1 FROM computer_objects o WHERE o.org_id=c.org_id AND o.digest=c.digest)
 AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.org_id=c.org_id AND a.digest=c.digest);

-- The caller has removed an abandoned graph owner in this transaction, not
-- merely observed an arbitrary temporarily unowned upload. Permanent retirement
-- prevents late upload/adoption from reviving the same physical key.
-- name: RetireCollectedComputerObject :execrows
UPDATE cas_object_lifetimes l SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp()
 WHERE l.digest=sqlc.arg(digest) AND l.retired_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM cas_objects c WHERE c.digest=l.digest)
 AND NOT EXISTS (SELECT 1 FROM computer_objects o WHERE o.digest=l.digest)
 AND NOT EXISTS (SELECT 1 FROM run_checkpoint_objects c WHERE c.digest=l.digest AND c.checkpoint_status='creating');

-- Collectors deleting different logical owners of the same physical digest must
-- serialize membership cleanup. Acquire in a separate statement after object
-- deletion; subsequent statements then see the previous collector's commit.
-- NO KEY UPDATE avoids blocking ordinary FK acquisition until actual retirement.
-- name: LockCollectedComputerLifetime :one
SELECT digest FROM cas_object_lifetimes WHERE digest=sqlc.arg(digest) FOR NO KEY UPDATE;
