-- The owner validates the exact certified root page and holds the preparation
-- locks. Publication records success once; pending uploads remain Runtime pins.
-- name: PublishInitialComputerVersion :one
WITH published AS (
    UPDATE computer_versions v
       SET status='committed', published_at=clock_timestamp(),
           content_digest=sqlc.arg(content_digest), size_bytes=sqlc.arg(logical_bytes),
           publisher_runtime_instance_id=sqlc.arg(runtime_instance_id),
           publisher_desired_version=sqlc.arg(desired_version),
           publication_request_fingerprint=sqlc.arg(fingerprint)
      FROM computers c
     WHERE v.environment_id=sqlc.arg(environment_id) AND v.workspace_id=sqlc.arg(computer_id)
       AND v.id=sqlc.arg(version_id) AND v.status='initializing' AND v.parent_version_id IS NULL
       AND c.environment_id=v.environment_id AND c.id=v.workspace_id
       AND c.head_version_id=v.id AND c.initial_config IS NULL
    RETURNING v.*
), retained AS (
    INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator)
    SELECT environment_id,workspace_id,id,sqlc.arg(locator) FROM published
    RETURNING version_id
), configured AS (
    UPDATE computers c SET initial_config=sqlc.arg(initial_config), updated_at=p.published_at
      FROM published p, retained r
     WHERE c.environment_id=p.environment_id AND c.id=p.workspace_id AND r.version_id=p.id
    RETURNING c.id
)
SELECT p.* FROM published p JOIN configured c ON c.id=p.workspace_id;

-- Authentication supplies the original Worker identity. Historical success is
-- independent of current head/config, desired state and retained payload lifetime.
-- name: GetWorkerInitialComputerVersion :one
SELECT v.* FROM computer_versions v
 JOIN runtime_instances r ON r.id=v.publisher_runtime_instance_id
 WHERE v.publisher_runtime_instance_id=sqlc.arg(runtime_instance_id)
   AND v.publisher_desired_version=sqlc.arg(desired_version)
   AND r.worker_instance_id=sqlc.arg(worker_instance_id)
   AND r.worker_group_id=sqlc.arg(worker_group_id) AND r.worker_epoch=sqlc.arg(worker_epoch);
