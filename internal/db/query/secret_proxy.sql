-- name: CaptureProtectedSecretEnvelopes :many
-- One primary statement snapshot captures both authorization and ciphertext.
-- claims_current reports credential freshness on the same snapshot; it is not
-- authority, and the caller answers a stale credential before using material.
WITH authority AS (
 SELECT i.computer_id,i.environment_id,c.secret_ca_certificate AS certificate,
 c.secret_ca_not_after AS not_after,statement_timestamp()::timestamptz AS authorized_at,
 COALESCE(h.claim_version=sqlc.arg(claim_version) AND g.claim_version=sqlc.arg(group_claim_version),false)::boolean AS claims_current
 FROM computer_instances i
 JOIN computers c ON c.id=i.computer_id AND c.environment_id=i.environment_id AND c.writer_generation=i.writer_generation
 JOIN worker_hosts h ON h.id=i.worker_host_id AND h.worker_group_id=i.worker_group_id AND h.current_epoch=i.worker_epoch
 JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE i.id=sqlc.arg(computer_instance_id) AND i.worker_host_id=sqlc.arg(worker_host_id)
 AND i.worker_epoch=sqlc.arg(worker_epoch) AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND h.status IN ('active','draining') AND g.status IN ('active','paused','draining')
 AND h.observed_at>=statement_timestamp()-interval '120 seconds'
 AND c.status='active' AND c.desired_state='active' AND c.deleted_at IS NULL
 AND i.desired_state='ready' AND i.reclaimed_at IS NULL AND i.writer_expires_at>statement_timestamp()
 AND i.observed_state='ready' AND i.mount_state='mounted' AND i.guest_channel_token_expires_at>statement_timestamp()
 AND (EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_instance_id=i.id AND l.writer_generation=i.writer_generation
             AND l.status IN ('starting','running') AND l.expires_at>statement_timestamp())
      OR EXISTS(SELECT 1 FROM computer_commands command WHERE command.computer_instance_id=i.id
                 AND command.writer_generation=i.writer_generation AND command.status IN ('starting','running')))
)
SELECT b.placeholder,a.environment_id,s.id AS secret_id,v.id AS version_id,v.version,v.nonce,v.ciphertext,
 a.certificate,a.not_after,a.authorized_at,a.claims_current
FROM authority a JOIN computer_secrets b ON b.computer_id=a.computer_id AND b.environment_id=a.environment_id
JOIN secrets s ON s.id=b.secret_id AND s.environment_id=a.environment_id AND s.status='active'
JOIN secret_versions v ON v.secret_id=s.id AND v.id=s.current_version_id
WHERE b.placement_kind='env' AND b.mode='protected' AND b.placeholder=ANY(sqlc.arg(placeholders)::text[])
 AND sqlc.arg(origin)::text=ANY(b.allowed_origins) ORDER BY b.placeholder;

-- name: CaptureSecretProxyPreparation :one
-- Computer CA creation is separate; preparation captures only existing material.
-- claims_current has the same freshness-only meaning as in protected capture.
-- An allocated Instance is still being admitted and needs admitting supply
-- (active Group, Pool and Host without Run or VM pauses); a ready Instance
-- continues on paused or draining supply.
WITH authority AS (
 SELECT i.computer_id,i.environment_id,c.secret_ca_certificate AS certificate,c.secret_ca_not_after AS not_after,
 c.secret_ca_private_key_nonce AS private_key_nonce,c.secret_ca_private_key_ciphertext AS private_key_ciphertext,
 COALESCE(h.claim_version=sqlc.arg(claim_version) AND g.claim_version=sqlc.arg(group_claim_version),false)::boolean AS claims_current
 FROM computer_instances i
 JOIN computers c ON c.id=i.computer_id AND c.environment_id=i.environment_id AND c.writer_generation=i.writer_generation
 JOIN worker_hosts h ON h.id=i.worker_host_id AND h.worker_group_id=i.worker_group_id AND h.current_epoch=i.worker_epoch
 JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE i.id=sqlc.arg(computer_instance_id) AND i.worker_host_id=sqlc.arg(worker_host_id)
 AND i.worker_epoch=sqlc.arg(worker_epoch) AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND h.status IN ('active','draining') AND g.status IN ('active','paused','draining')
 AND h.observed_at>=statement_timestamp()-interval '120 seconds'
 AND c.status='active' AND c.desired_state='active' AND c.deleted_at IS NULL
 AND i.desired_state='ready' AND i.reclaimed_at IS NULL AND i.writer_expires_at>statement_timestamp()
 AND ((i.observed_state='allocated' AND i.preparation_expires_at>statement_timestamp() AND h.status='active' AND g.status='active'
       AND h.run_paused_reason IS NULL AND h.vm_paused_reason IS NULL
       AND EXISTS(SELECT 1 FROM worker_pools p WHERE p.id=h.worker_pool_id AND p.status='active'))
      OR (i.observed_state='ready' AND i.mount_state='mounted' AND i.guest_channel_token_expires_at>statement_timestamp()))
)
SELECT a.environment_id,a.computer_id,a.certificate,a.not_after,a.private_key_nonce,a.private_key_ciphertext,a.claims_current,
 ARRAY(SELECT DISTINCT unnest(b.allowed_origins) FROM computer_secrets b
        WHERE b.computer_id=a.computer_id AND b.environment_id=a.environment_id
         AND b.placement_kind='env' AND b.mode='protected')::text[] AS origins
FROM authority a;

-- name: GetComputerSecretCAPublic :one
SELECT secret_ca_certificate AS certificate, secret_ca_not_after AS not_after
FROM computers WHERE environment_id = sqlc.arg(environment_id) AND id = sqlc.arg(computer_id);

-- name: InitializeComputerSecretCA :execrows
-- Creation-only: caller owns the insert transaction and uses inserted created_at.
-- No preparation or later lifecycle operation may initialize or replace a CA.
UPDATE computers SET secret_ca_certificate = sqlc.arg(certificate),
 secret_ca_private_key_nonce = sqlc.arg(private_key_nonce),
 secret_ca_private_key_ciphertext = sqlc.arg(private_key_ciphertext),
 secret_ca_not_after = sqlc.arg(not_after)
WHERE environment_id = sqlc.arg(environment_id) AND id = sqlc.arg(computer_id)
 AND secret_ca_certificate IS NULL;
