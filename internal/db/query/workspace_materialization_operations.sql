-- name: RequestWorkspaceMaterializationOperation :one
WITH active_materialization AS MATERIALIZED (
    SELECT *
      FROM workspace_materializations
     WHERE workspace_materializations.org_id = sqlc.arg(org_id)
       AND workspace_materializations.project_id = sqlc.arg(project_id)
       AND workspace_materializations.environment_id = sqlc.arg(environment_id)
       AND workspace_materializations.workspace_id = sqlc.arg(workspace_id)
       AND workspace_materializations.id = sqlc.arg(materialization_id)
       AND workspace_materializations.state IN ('running', 'paused')
),
active_write_lease AS MATERIALIZED (
    SELECT workspace_leases.id,
           workspace_leases.acquired_fencing_generation
      FROM active_materialization
      JOIN workspace_leases
        ON workspace_leases.org_id = active_materialization.org_id
       AND workspace_leases.project_id = active_materialization.project_id
       AND workspace_leases.environment_id = active_materialization.environment_id
       AND workspace_leases.workspace_id = active_materialization.workspace_id
       AND workspace_leases.materialization_id = active_materialization.id
     WHERE sqlc.narg(write_lease_id)::uuid IS NOT NULL
       AND workspace_leases.id = sqlc.narg(write_lease_id)::uuid
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.fencing_token = coalesce(sqlc.arg(fencing_token)::text, '')
       AND workspace_leases.expires_at > now()
)
INSERT INTO workspace_materialization_operations (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    materialization_id,
    operation_kind,
    resource_kind,
    resource_id,
    request_fingerprint,
    operation_expires_at,
    priority,
    instance_lease_id,
    write_lease_id,
    fencing_token,
    fencing_generation,
    request
)
SELECT sqlc.arg(id),
       active_materialization.org_id,
       active_materialization.project_id,
       active_materialization.environment_id,
       active_materialization.workspace_id,
       active_materialization.id,
       sqlc.arg(operation_kind),
       coalesce(sqlc.arg(resource_kind)::text, ''),
       sqlc.narg(resource_id),
       sqlc.arg(request_fingerprint),
       sqlc.arg(operation_expires_at),
       sqlc.arg(priority),
       sqlc.narg(instance_lease_id),
       sqlc.narg(write_lease_id),
       coalesce(sqlc.arg(fencing_token)::text, ''),
       coalesce(active_write_lease.acquired_fencing_generation, active_materialization.fencing_generation),
       coalesce(sqlc.arg(request)::jsonb, '{}'::jsonb)
  FROM active_materialization
  LEFT JOIN active_write_lease ON true
 WHERE (
       (
           sqlc.narg(write_lease_id)::uuid IS NULL
           AND coalesce(sqlc.arg(fencing_token)::text, '') = ''
       )
       OR active_write_lease.id IS NOT NULL
   )
   AND (
       sqlc.arg(operation_kind)::text NOT IN ('StartExec', 'CreatePty', 'ResizePty', 'ClosePty')
       OR active_write_lease.id IS NOT NULL
   )
RETURNING *;

-- name: GetWorkspaceMaterializationOperation :one
SELECT *
  FROM workspace_materialization_operations
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: GetActiveWorkspaceMaterializationOperationByResource :one
SELECT *
  FROM workspace_materialization_operations
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND materialization_id = sqlc.arg(materialization_id)
   AND operation_kind = sqlc.arg(operation_kind)
   AND resource_kind = sqlc.arg(resource_kind)
   AND resource_id = sqlc.arg(resource_id)
   AND state IN ('queued', 'claimed', 'running')
 ORDER BY requested_at ASC
 LIMIT 1;

-- name: ClaimWorkspaceMaterializationOperation :one
WITH exhausted AS (
    UPDATE workspace_materialization_operations
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_operation_claims_exhausted'),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.materialization_id = sqlc.arg(materialization_id)
       AND workspace_materialization_operations.state = 'claimed'
       AND workspace_materialization_operations.claim_expires_at <= now()
       AND workspace_materialization_operations.claim_attempt >= sqlc.arg(max_claim_attempts)
    RETURNING workspace_materialization_operations.id
),
expired AS (
    UPDATE workspace_materialization_operations
       SET state = 'expired',
           error = jsonb_build_object('code', 'workspace_operation_expired'),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.materialization_id = sqlc.arg(materialization_id)
       AND workspace_materialization_operations.state IN ('queued', 'claimed', 'running')
       AND workspace_materialization_operations.operation_expires_at <= now()
    RETURNING workspace_materialization_operations.id
),
candidate AS (
    SELECT workspace_materialization_operations.id
      FROM workspace_materialization_operations
      JOIN workspace_materializations
        ON workspace_materializations.org_id = workspace_materialization_operations.org_id
       AND workspace_materializations.project_id = workspace_materialization_operations.project_id
       AND workspace_materializations.environment_id = workspace_materialization_operations.environment_id
       AND workspace_materializations.workspace_id = workspace_materialization_operations.workspace_id
       AND workspace_materializations.id = workspace_materialization_operations.materialization_id
     WHERE workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.materialization_id = sqlc.arg(materialization_id)
       AND (
           workspace_materialization_operations.state = 'queued'
           OR (
               workspace_materialization_operations.state = 'claimed'
               AND workspace_materialization_operations.claim_expires_at <= now()
           )
       )
       AND workspace_materialization_operations.claim_attempt < sqlc.arg(max_claim_attempts)
	       AND workspace_materialization_operations.operation_expires_at > now()
	       AND workspace_materializations.worker_instance_id = sqlc.arg(worker_instance_id)
	       AND workspace_materializations.reservation_token = sqlc.arg(reservation_token)
	       AND workspace_materializations.state IN ('running', 'paused')
	       AND (
	           workspace_materialization_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_materialization_operations.org_id
	                  AND workspace_leases.project_id = workspace_materialization_operations.project_id
	                  AND workspace_leases.environment_id = workspace_materialization_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_materialization_operations.workspace_id
	                  AND workspace_leases.materialization_id = workspace_materialization_operations.materialization_id
	                  AND workspace_leases.id = workspace_materialization_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_materialization_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_materialization_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
	     ORDER BY workspace_materialization_operations.priority DESC,
              workspace_materialization_operations.requested_at ASC
     LIMIT 1
     FOR UPDATE SKIP LOCKED
)
UPDATE workspace_materialization_operations
   SET state = 'claimed',
       claimed_by_worker_instance_id = sqlc.arg(worker_instance_id),
       claim_token = sqlc.arg(claim_token),
       claim_attempt = workspace_materialization_operations.claim_attempt + 1,
       claim_expires_at = sqlc.arg(claim_expires_at),
       claimed_at = now(),
       updated_at = now()
 FROM candidate
 WHERE workspace_materialization_operations.id = candidate.id
   AND (
       workspace_materialization_operations.state = 'queued'
       OR (
           workspace_materialization_operations.state = 'claimed'
           AND workspace_materialization_operations.claim_expires_at <= now()
       )
   )
RETURNING workspace_materialization_operations.*;

-- name: StartWorkspaceMaterializationOperation :one
WITH started AS (
    UPDATE workspace_materialization_operations
       SET state = 'running',
           updated_at = now()
     WHERE workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.id = sqlc.arg(id)
       AND workspace_materialization_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_materialization_operations.claim_token = sqlc.arg(claim_token)
	       AND workspace_materialization_operations.state = 'claimed'
	       AND workspace_materialization_operations.claim_expires_at > now()
	       AND workspace_materialization_operations.operation_expires_at > now()
	       AND (
	           workspace_materialization_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_materialization_operations.org_id
	                  AND workspace_leases.project_id = workspace_materialization_operations.project_id
	                  AND workspace_leases.environment_id = workspace_materialization_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_materialization_operations.workspace_id
	                  AND workspace_leases.materialization_id = workspace_materialization_operations.materialization_id
	                  AND workspace_leases.id = workspace_materialization_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_materialization_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_materialization_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
    RETURNING *
)
SELECT * FROM started
UNION ALL
SELECT *
  FROM workspace_materialization_operations
 WHERE NOT EXISTS (SELECT 1 FROM started)
   AND workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.id = sqlc.arg(id)
       AND workspace_materialization_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
	       AND workspace_materialization_operations.claim_token = sqlc.arg(claim_token)
	       AND workspace_materialization_operations.state = 'running'
	       AND workspace_materialization_operations.operation_expires_at > now()
	       AND (
	           workspace_materialization_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_materialization_operations.org_id
	                  AND workspace_leases.project_id = workspace_materialization_operations.project_id
	                  AND workspace_leases.environment_id = workspace_materialization_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_materialization_operations.workspace_id
	                  AND workspace_leases.materialization_id = workspace_materialization_operations.materialization_id
	                  AND workspace_leases.id = workspace_materialization_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_materialization_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_materialization_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
LIMIT 1;

-- name: CompleteWorkspaceMaterializationOperation :one
WITH completed AS (
    UPDATE workspace_materialization_operations
       SET state = 'completed',
           result = coalesce(sqlc.arg(result)::jsonb, '{}'::jsonb),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.id = sqlc.arg(id)
       AND workspace_materialization_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_materialization_operations.claim_token = sqlc.arg(claim_token)
	       AND workspace_materialization_operations.state = 'running'
	       AND workspace_materialization_operations.operation_expires_at > now()
	       AND (
	           workspace_materialization_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_materialization_operations.org_id
	                  AND workspace_leases.project_id = workspace_materialization_operations.project_id
	                  AND workspace_leases.environment_id = workspace_materialization_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_materialization_operations.workspace_id
	                  AND workspace_leases.materialization_id = workspace_materialization_operations.materialization_id
	                  AND workspace_leases.id = workspace_materialization_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_materialization_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_materialization_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
    RETURNING *
)
SELECT * FROM completed
UNION ALL
SELECT *
  FROM workspace_materialization_operations
 WHERE NOT EXISTS (SELECT 1 FROM completed)
   AND workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.id = sqlc.arg(id)
       AND workspace_materialization_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_materialization_operations.claim_token = sqlc.arg(claim_token)
       AND workspace_materialization_operations.state = 'completed'
   AND workspace_materialization_operations.result = coalesce(sqlc.arg(result)::jsonb, '{}'::jsonb)
LIMIT 1;

-- name: FailWorkspaceMaterializationOperation :one
WITH failed AS (
    UPDATE workspace_materialization_operations
       SET state = 'failed',
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_materialization_operations.org_id = sqlc.arg(org_id)
       AND workspace_materialization_operations.id = sqlc.arg(id)
       AND workspace_materialization_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_materialization_operations.claim_token = sqlc.arg(claim_token)
       AND workspace_materialization_operations.state IN ('claimed', 'running')
	       AND (
	           workspace_materialization_operations.state = 'running'
	           OR workspace_materialization_operations.claim_expires_at > now()
	       )
	       AND workspace_materialization_operations.operation_expires_at > now()
	       AND (
	           workspace_materialization_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_materialization_operations.org_id
	                  AND workspace_leases.project_id = workspace_materialization_operations.project_id
	                  AND workspace_leases.environment_id = workspace_materialization_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_materialization_operations.workspace_id
	                  AND workspace_leases.materialization_id = workspace_materialization_operations.materialization_id
	                  AND workspace_leases.id = workspace_materialization_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_materialization_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_materialization_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
    RETURNING *
)
SELECT * FROM failed
UNION ALL
SELECT *
  FROM workspace_materialization_operations
 WHERE NOT EXISTS (SELECT 1 FROM failed)
   AND workspace_materialization_operations.org_id = sqlc.arg(org_id)
   AND workspace_materialization_operations.id = sqlc.arg(id)
   AND workspace_materialization_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_materialization_operations.claim_token = sqlc.arg(claim_token)
   AND workspace_materialization_operations.state = 'failed'
   AND workspace_materialization_operations.error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb)
LIMIT 1;
