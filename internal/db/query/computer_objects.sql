-- name: LockComputerObject :one
SELECT * FROM computer_objects
 WHERE environment_id=sqlc.arg(environment_id)
   AND digest=sqlc.arg(digest)
 FOR UPDATE;

-- The data-modifying CTE always runs in the same statement. Certification does
-- not depend on its inserted row count: every inherited key may already be direct.
-- name: CertifyComputerObject :execrows
WITH object AS MATERIALIZED (
 SELECT o.* FROM computer_objects o
 WHERE o.environment_id=sqlc.arg(environment_id)
   AND o.digest=sqlc.arg(digest) AND NOT o.certified
   AND EXISTS(SELECT 1 FROM computer_object_keys k WHERE k.environment_id=o.environment_id AND k.digest=o.digest AND k.is_direct)
 FOR UPDATE
), summary AS (
 INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct)
 SELECT DISTINCT o.environment_id,o.digest,k.key_id,false
 FROM object o
 JOIN computer_object_edges e ON e.environment_id=o.environment_id
   AND e.parent_digest=o.digest
 JOIN computer_object_keys k ON k.environment_id=e.environment_id
   AND k.digest=e.child_digest
 ON CONFLICT (environment_id,digest,key_id) DO NOTHING
 RETURNING key_id
)
UPDATE computer_objects o SET certified_at=clock_timestamp()
 FROM object selected
 WHERE o.environment_id=selected.environment_id
   AND o.digest=selected.digest;
