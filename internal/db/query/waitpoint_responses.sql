-- name: RecordWaitpointResponse :one
WITH target_waitpoint AS (
    SELECT *
      FROM waitpoints
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.kind = sqlc.arg(kind)
       AND waitpoints.status IN ('pending', 'completed')
       AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now())
     FOR UPDATE
),
existing_response AS (
    SELECT waitpoint_responses.*
      FROM waitpoint_responses
      JOIN target_waitpoint ON target_waitpoint.org_id = waitpoint_responses.org_id
                           AND target_waitpoint.id = waitpoint_responses.waitpoint_id
     WHERE waitpoint_responses.response_key = sqlc.arg(response_key)
     FOR UPDATE OF waitpoint_responses
),
inserted_response AS (
    INSERT INTO waitpoint_responses (
        id,
        org_id,
        project_id,
        environment_id,
        waitpoint_id,
        response_key,
        request_hash,
        action,
        resolution_kind,
        resolution,
        event_payload,
        completed_by_principal,
        completed_via,
        external_subject,
        metadata
    )
    SELECT
        sqlc.arg(id),
        target_waitpoint.org_id,
        target_waitpoint.project_id,
        target_waitpoint.environment_id,
        target_waitpoint.id,
        sqlc.arg(response_key),
        sqlc.arg(request_hash),
        sqlc.arg(action),
        sqlc.arg(resolution_kind),
        sqlc.arg(resolution),
        sqlc.arg(event_payload)::jsonb,
        sqlc.narg(completed_by_principal),
        sqlc.narg(completed_via),
        sqlc.narg(external_subject),
        sqlc.arg(metadata)::jsonb
      FROM target_waitpoint
     WHERE target_waitpoint.status = 'pending'
       AND NOT EXISTS (SELECT 1 FROM existing_response)
    RETURNING *
),
matching_existing_response AS (
    SELECT existing_response.*
      FROM existing_response
     WHERE existing_response.request_hash = sqlc.arg(request_hash)
)
SELECT * FROM inserted_response
UNION ALL
SELECT * FROM matching_existing_response
LIMIT 1;
