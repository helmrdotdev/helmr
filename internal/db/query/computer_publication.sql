-- The owner validates the exact certified root and holds preparation locks.
-- An adopted initial version has no source writer. Its Instance retains the
-- request receipt independently of the version's payload lifetime.
-- name: PublishInitialComputerDiskVersion :one
WITH published AS (
 UPDATE computer_disk_versions v SET status='committed',published_at=clock_timestamp()
 FROM computers c
 WHERE v.environment_id=sqlc.arg(environment_id) AND v.computer_id=sqlc.arg(computer_id)
 AND v.id=sqlc.arg(version_id) AND v.status='initializing' AND v.parent_version_id IS NULL
 AND c.environment_id=v.environment_id AND c.id=v.computer_id
 AND c.head_disk_version_id=v.id AND c.initial_config IS NULL
 RETURNING v.*
), retained AS (
 INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,root_id)
 SELECT environment_id,computer_id,id,sqlc.arg(root_id) FROM published RETURNING version_id
), configured AS (
 UPDATE computers c SET initial_config=sqlc.arg(initial_config),updated_at=p.published_at
 FROM published p,retained r WHERE c.environment_id=p.environment_id AND c.id=p.computer_id AND r.version_id=p.id
 RETURNING c.id
), receipt AS (
 UPDATE computer_instances i SET initial_disk_version_id=p.id,
 initial_publication_desired_version=sqlc.arg(desired_version),initial_publication_fingerprint=sqlc.arg(fingerprint)
 FROM published p,configured c WHERE i.id=sqlc.arg(computer_instance_id) AND i.computer_id=c.id
 AND i.initial_disk_version_id IS NULL RETURNING i.id
)
SELECT p.* FROM published p,receipt;

-- Authentication supplies the original Worker identity; no live authority or
-- retained root is needed to replay a committed initial adoption.
-- name: GetWorkerInitialComputerDiskVersion :one
SELECT v.id,v.computer_id,r.initial_publication_fingerprint AS publication_request_fingerprint
 FROM computer_instances r JOIN computer_disk_versions v ON v.id=r.initial_disk_version_id
 WHERE r.id=sqlc.arg(computer_instance_id)
 AND r.initial_publication_desired_version=sqlc.arg(desired_version)
 AND r.worker_host_id=sqlc.arg(worker_host_id)
 AND r.worker_group_id=sqlc.arg(worker_group_id) AND r.worker_epoch=sqlc.arg(worker_epoch);
