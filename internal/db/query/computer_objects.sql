-- The caller owns the fenced publication transaction. These storage operations
-- neither authenticate a Worker nor accept an unverified ciphertext declaration.
-- Admission/edge/key mutation must lock the parent and reject certified objects;
-- descriptors and immediate dependencies are immutable after certification.

-- name: LockComputerObject :one
SELECT * FROM computer_objects
 WHERE environment_id=sqlc.arg(environment_id) AND computer_id=sqlc.arg(computer_id)
   AND digest=sqlc.arg(digest)
 FOR UPDATE;

-- name: CertifyComputerObject :execrows
WITH object AS MATERIALIZED (
 SELECT o.* FROM computer_objects o
 WHERE o.environment_id=sqlc.arg(environment_id) AND o.computer_id=sqlc.arg(computer_id)
   AND o.digest=sqlc.arg(digest) AND NOT o.certified
   AND EXISTS(SELECT 1 FROM computer_object_keys k WHERE k.environment_id=o.environment_id AND k.computer_id=o.computer_id AND k.digest=o.digest)
 FOR UPDATE
), summary AS (
 INSERT INTO computer_object_read_keys(environment_id,computer_id,digest,key_id)
 SELECT o.environment_id,o.computer_id,o.digest,keys.key_id
 FROM object o CROSS JOIN LATERAL (
   SELECT k.key_id FROM computer_object_keys k
    WHERE k.environment_id=o.environment_id AND k.computer_id=o.computer_id AND k.digest=o.digest
   UNION
   SELECT k.key_id FROM computer_object_edges e
    JOIN computer_object_read_keys k ON k.environment_id=e.environment_id
      AND k.computer_id=e.computer_id AND k.digest=e.child_digest
    WHERE e.environment_id=o.environment_id AND e.computer_id=o.computer_id AND e.parent_digest=o.digest
 ) keys
 RETURNING key_id
)
UPDATE computer_objects o SET certified_at=clock_timestamp()
 FROM object selected
 WHERE o.environment_id=selected.environment_id AND o.computer_id=selected.computer_id
   AND o.digest=selected.digest AND EXISTS(SELECT 1 FROM summary);

-- The root comes from the exact retained owner, never an arbitrary HTTP key list.
-- name: ListComputerObjectReadKeys :many
SELECT k.* FROM computer_object_read_keys r
 JOIN computer_objects o USING(environment_id,computer_id,digest)
 JOIN computer_keys k ON k.environment_id=r.environment_id AND k.computer_id=r.computer_id AND k.id=r.key_id
 WHERE r.environment_id=sqlc.arg(environment_id) AND r.computer_id=sqlc.arg(computer_id)
   AND r.digest=sqlc.arg(digest) AND o.certified AND k.available
 ORDER BY k.id;
