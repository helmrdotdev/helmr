-- name: CaptureProtectedSecretEnvelopes :many
-- One primary command snapshot is the authorization point. No later mutable
-- lookup may supply material for these captured envelopes.
WITH authority AS (
 SELECT r.workspace_id, r.environment_id, r.reservation_expires_at, r.reserved_run_id, r.reserved_process_id,
 w.secret_ca_certificate AS certificate, w.secret_ca_not_after AS not_after, statement_timestamp()::timestamptz AS authorized_at,
 EXISTS (
  SELECT 1 FROM workspace_leases l
  JOIN workspace_mounts m ON m.id = l.workspace_mount_id
   AND m.runtime_instance_id = r.id AND m.workspace_id = r.workspace_id
   AND m.environment_id = r.environment_id AND m.org_id = r.org_id AND m.project_id = r.project_id
   AND m.worker_group_id = r.worker_group_id AND m.worker_instance_id = r.worker_instance_id
   AND m.worker_epoch = r.worker_epoch AND m.region_id = r.region_id
  WHERE l.runtime_instance_id = r.id AND l.workspace_id = r.workspace_id
   AND l.environment_id = r.environment_id AND l.org_id = r.org_id AND l.project_id = r.project_id
   AND l.worker_group_id = r.worker_group_id AND l.worker_instance_id = r.worker_instance_id
   AND l.worker_epoch = r.worker_epoch AND l.region_id = r.region_id
   AND l.status = 'active' AND l.expires_at > statement_timestamp()
   AND l.ownership_generation = w.ownership_generation AND l.writer_generation = w.writer_generation
   AND m.status = 'mounted' AND m.fencing_generation = l.mount_fencing_generation
   AND m.materialized_version_id = l.base_workspace_version_id
   AND m.guest_channel_token_expires_at > statement_timestamp()
   AND r.observed_state = 'ready'
   AND (
    (l.owner_process_id IS NULL AND EXISTS (
     SELECT 1 FROM run_leases owner
     WHERE owner.id = l.owner_run_lease_id AND owner.runtime_instance_id = r.id
      AND owner.workspace_id = r.workspace_id AND owner.environment_id = r.environment_id
      AND owner.org_id = r.org_id AND owner.project_id = r.project_id AND owner.region_id = r.region_id
      AND owner.worker_instance_id = r.worker_instance_id AND owner.worker_group_id = r.worker_group_id
      AND owner.worker_epoch = r.worker_epoch AND owner.runtime_identity_id = r.runtime_identity_id
      AND owner.status IN ('starting', 'running') AND owner.expires_at > statement_timestamp()
    ))
    OR (l.owner_run_lease_id IS NULL AND EXISTS (
     SELECT 1 FROM workspace_processes owner
     WHERE owner.id = l.owner_process_id AND owner.runtime_instance_id = r.id
      AND owner.workspace_mount_id = m.id AND owner.base_workspace_version_id = l.base_workspace_version_id
      AND owner.workspace_id = r.workspace_id AND owner.environment_id = r.environment_id
      AND owner.org_id = r.org_id AND owner.project_id = r.project_id AND owner.region_id = r.region_id
      AND owner.worker_instance_id = r.worker_instance_id AND owner.worker_group_id = r.worker_group_id
      AND owner.worker_epoch = r.worker_epoch AND owner.status IN ('starting', 'running')
    ))
   )
 ) AS live
 FROM runtime_instances r
 JOIN workspaces w ON w.id = r.workspace_id AND w.environment_id = r.environment_id
 JOIN worker_instances worker ON worker.id = r.worker_instance_id AND worker.worker_group_id = r.worker_group_id
 JOIN worker_groups worker_group ON worker_group.id = worker.worker_group_id
 WHERE r.id = sqlc.arg(runtime_instance_id)
  AND r.worker_instance_id = sqlc.arg(worker_instance_id) AND r.worker_epoch = sqlc.arg(worker_epoch)
  AND worker.worker_group_id = sqlc.arg(worker_group_id)
  AND worker.current_epoch = r.worker_epoch AND worker.claim_version = sqlc.arg(claim_version)
  AND worker_group.claim_version = sqlc.arg(group_claim_version)
  AND worker.status IN ('active', 'draining') AND worker_group.status IN ('active', 'draining')
  AND worker.observed_at >= statement_timestamp() - interval '120 seconds'
  AND r.desired_state = 'ready' AND r.observed_state IN ('allocated', 'ready') AND r.reclaimed_at IS NULL
  AND w.status = 'active' AND w.desired_state = 'active' AND w.deleted_at IS NULL
)
SELECT b.placeholder, a.environment_id, s.id AS secret_id,
 v.id AS version_id, v.version, v.nonce, v.ciphertext,
 a.certificate, a.not_after, a.authorized_at
FROM authority a
JOIN workspace_secrets b ON b.workspace_id = a.workspace_id AND b.environment_id = a.environment_id
JOIN secrets s ON s.id = b.secret_id AND s.environment_id = a.environment_id AND s.status = 'active'
JOIN secret_versions v ON v.secret_id = s.id AND v.id = s.current_version_id
WHERE a.live AND b.placement_kind = 'env' AND b.mode = 'protected'
 AND b.placeholder = ANY(sqlc.arg(placeholders)::text[])
 AND sqlc.arg(origin)::text = ANY(b.allowed_origins)
ORDER BY b.placeholder;

-- name: CaptureSecretProxyPreparation :one
-- TLS preparation is reservation-or-live, distinct from credential use.
-- The root is persisted at Workspace creation; preparation only captures existing material.
WITH authority AS (
 SELECT r.workspace_id, r.environment_id, r.reservation_expires_at, r.reserved_run_id, r.reserved_process_id,
 w.secret_ca_certificate AS certificate, w.secret_ca_not_after AS not_after,
 w.secret_ca_private_key_nonce AS private_key_nonce, w.secret_ca_private_key_ciphertext AS private_key_ciphertext, statement_timestamp()::timestamptz AS authorized_at,
 EXISTS (
  SELECT 1 FROM workspace_leases l
  JOIN workspace_mounts m ON m.id = l.workspace_mount_id
   AND m.runtime_instance_id = r.id AND m.workspace_id = r.workspace_id
   AND m.environment_id = r.environment_id AND m.org_id = r.org_id AND m.project_id = r.project_id
   AND m.worker_group_id = r.worker_group_id AND m.worker_instance_id = r.worker_instance_id
   AND m.worker_epoch = r.worker_epoch AND m.region_id = r.region_id
  WHERE l.runtime_instance_id = r.id AND l.workspace_id = r.workspace_id
   AND l.environment_id = r.environment_id AND l.org_id = r.org_id AND l.project_id = r.project_id
   AND l.worker_group_id = r.worker_group_id AND l.worker_instance_id = r.worker_instance_id
   AND l.worker_epoch = r.worker_epoch AND l.region_id = r.region_id
   AND l.status = 'active' AND l.expires_at > statement_timestamp()
   AND l.ownership_generation = w.ownership_generation AND l.writer_generation = w.writer_generation
   AND m.status = 'mounted' AND m.fencing_generation = l.mount_fencing_generation
   AND m.materialized_version_id = l.base_workspace_version_id
   AND m.guest_channel_token_expires_at > statement_timestamp()
   AND r.observed_state = 'ready'
   AND (
    (l.owner_process_id IS NULL AND EXISTS (
     SELECT 1 FROM run_leases owner
     WHERE owner.id = l.owner_run_lease_id AND owner.runtime_instance_id = r.id
      AND owner.workspace_id = r.workspace_id AND owner.environment_id = r.environment_id
      AND owner.org_id = r.org_id AND owner.project_id = r.project_id AND owner.region_id = r.region_id
      AND owner.worker_instance_id = r.worker_instance_id AND owner.worker_group_id = r.worker_group_id
      AND owner.worker_epoch = r.worker_epoch AND owner.runtime_identity_id = r.runtime_identity_id
      AND owner.status IN ('starting', 'running') AND owner.expires_at > statement_timestamp()
    ))
    OR (l.owner_run_lease_id IS NULL AND EXISTS (
     SELECT 1 FROM workspace_processes owner
     WHERE owner.id = l.owner_process_id AND owner.runtime_instance_id = r.id
      AND owner.workspace_mount_id = m.id AND owner.base_workspace_version_id = l.base_workspace_version_id
      AND owner.workspace_id = r.workspace_id AND owner.environment_id = r.environment_id
      AND owner.org_id = r.org_id AND owner.project_id = r.project_id AND owner.region_id = r.region_id
      AND owner.worker_instance_id = r.worker_instance_id AND owner.worker_group_id = r.worker_group_id
      AND owner.worker_epoch = r.worker_epoch AND owner.status IN ('starting', 'running')
    ))
   )
 ) AS live
 FROM runtime_instances r
 JOIN workspaces w ON w.id = r.workspace_id AND w.environment_id = r.environment_id
 JOIN worker_instances worker ON worker.id = r.worker_instance_id AND worker.worker_group_id = r.worker_group_id
 JOIN worker_groups worker_group ON worker_group.id = worker.worker_group_id
 WHERE r.id = sqlc.arg(runtime_instance_id)
  AND r.worker_instance_id = sqlc.arg(worker_instance_id) AND r.worker_epoch = sqlc.arg(worker_epoch)
  AND worker.worker_group_id = sqlc.arg(worker_group_id)
  AND worker.current_epoch = r.worker_epoch AND worker.claim_version = sqlc.arg(claim_version)
  AND worker_group.claim_version = sqlc.arg(group_claim_version)
  AND worker.status IN ('active', 'draining') AND worker_group.status IN ('active', 'draining')
  AND worker.observed_at >= statement_timestamp() - interval '120 seconds'
  AND r.desired_state = 'ready' AND r.observed_state IN ('allocated', 'ready') AND r.reclaimed_at IS NULL
  AND w.status = 'active' AND w.desired_state = 'active' AND w.deleted_at IS NULL
)
SELECT a.environment_id, a.workspace_id, a.certificate, a.not_after, a.private_key_nonce, a.private_key_ciphertext,
 ARRAY(SELECT DISTINCT unnest(b.allowed_origins) FROM workspace_secrets b
       WHERE b.workspace_id = a.workspace_id AND b.environment_id = a.environment_id
        AND b.placement_kind = 'env' AND b.mode = 'protected')::text[] AS origins
FROM authority a
WHERE a.live OR (
 a.reservation_expires_at > a.authorized_at
 AND ((a.reserved_run_id IS NOT NULL) <> (a.reserved_process_id IS NOT NULL))
);

-- name: GetWorkspaceSecretCAPublic :one
SELECT secret_ca_certificate AS certificate, secret_ca_not_after AS not_after
FROM workspaces WHERE environment_id = sqlc.arg(environment_id) AND id = sqlc.arg(workspace_id);

-- name: InitializeWorkspaceSecretCA :execrows
-- Creation-only: caller owns the insert transaction and uses inserted created_at.
-- No preparation or later lifecycle operation may initialize or replace a CA.
UPDATE workspaces SET secret_ca_certificate = sqlc.arg(certificate),
 secret_ca_private_key_nonce = sqlc.arg(private_key_nonce),
 secret_ca_private_key_ciphertext = sqlc.arg(private_key_ciphertext),
 secret_ca_not_after = sqlc.arg(not_after)
WHERE environment_id = sqlc.arg(environment_id) AND id = sqlc.arg(workspace_id)
 AND secret_ca_certificate IS NULL;
