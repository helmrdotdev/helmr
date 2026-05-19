-- name: CreateWaitpointDelivery :one
INSERT INTO waitpoint_deliveries (
    id,
    org_id,
    run_id,
    waitpoint_id,
    response_token_id,
    channel,
    recipient_kind,
    recipient,
    status,
    metadata
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(run_id),
    sqlc.arg(waitpoint_id),
    sqlc.narg(response_token_id),
    sqlc.arg(channel),
    sqlc.arg(recipient_kind),
    sqlc.arg(recipient),
    sqlc.arg(status)::waitpoint_delivery_status,
    sqlc.arg(metadata)
)
RETURNING *;

-- name: MarkWaitpointDeliverySent :one
UPDATE waitpoint_deliveries
   SET status = 'sent',
       last_error = NULL,
       sent_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: MarkWaitpointDeliveryFailed :one
UPDATE waitpoint_deliveries
   SET status = 'failed',
       last_error = sqlc.arg(last_error)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: ListWaitpointDeliveries :many
SELECT *
  FROM waitpoint_deliveries
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND waitpoint_id = sqlc.arg(waitpoint_id)
 ORDER BY created_at ASC;
