-- name: CreateDeviceCode :one
INSERT INTO device_codes (
    id,
    org_id,
    user_code_hash,
    device_code_hash,
    expires_at,
    poll_interval_seconds
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(user_code_hash),
    sqlc.arg(device_code_hash),
    sqlc.arg(expires_at),
    sqlc.arg(poll_interval_seconds)
)
RETURNING *;

-- name: GetDeviceCodeByUserCodeHash :one
SELECT *
  FROM device_codes
 WHERE user_code_hash = sqlc.arg(user_code_hash);

-- name: ApproveDeviceCode :one
UPDATE device_codes
   SET status = 'approved',
       org_id = sqlc.arg(org_id),
       decided_by_user_id = sqlc.arg(user_id),
       decided_at = now()
 WHERE user_code_hash = sqlc.arg(user_code_hash)
   AND status = 'pending'
   AND expires_at > now()
RETURNING *;

-- name: DenyDeviceCode :one
UPDATE device_codes
   SET status = 'denied',
       org_id = sqlc.arg(org_id),
       decided_by_user_id = sqlc.arg(user_id),
       decided_at = now()
 WHERE user_code_hash = sqlc.arg(user_code_hash)
   AND status = 'pending'
   AND expires_at > now()
RETURNING *;

-- name: GetDeviceCodeForPoll :one
SELECT *
  FROM device_codes
 WHERE device_code_hash = sqlc.arg(device_code_hash);

-- name: ConsumeDeviceCode :one
UPDATE device_codes
   SET status = 'consumed',
       consumed_at = now()
 WHERE device_code_hash = sqlc.arg(device_code_hash)
   AND status = 'approved'
   AND consumed_at IS NULL
   AND expires_at > now()
RETURNING *;

