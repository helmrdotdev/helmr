-- name: ListRunWaitpoints :many
WITH cursor_wait AS (
    SELECT created_at, id
      FROM waitpoints
     WHERE org_id = sqlc.arg(org_id)
       AND run_id = sqlc.arg(run_id)
       AND id = sqlc.narg(after_id)::uuid
)
SELECT waitpoints.id,
       waitpoints.org_id,
       waitpoints.project_id,
       waitpoints.environment_id,
       waitpoints.run_id,
       waitpoints.params,
       waitpoints.metadata,
       waitpoints.tags,
       waitpoints.waitpoint_token_id,
       waitpoints.data,
       waitpoints.error,
       waitpoints.resolved_at,
       waitpoints.created_at,
       waitpoints.updated_at,
       waitpoints.kind,
       CASE waitpoints.status
           WHEN 'pending' THEN 'pending'
           WHEN 'completed' THEN 'completed'
           WHEN 'timed_out' THEN 'timed_out'
           WHEN 'cancelled' THEN 'cancelled'
           ELSE 'failed'
       END::text AS status,
       run_suspension_waitpoints.timeout_seconds
  FROM waitpoints
  LEFT JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = waitpoints.org_id
                                       AND run_suspension_waitpoints.waitpoint_id = waitpoints.id
  LEFT JOIN run_suspensions ON run_suspensions.org_id = run_suspension_waitpoints.org_id
                           AND run_suspensions.id = run_suspension_waitpoints.run_suspension_id
	 WHERE waitpoints.org_id = sqlc.arg(org_id)
	   AND waitpoints.run_id = sqlc.arg(run_id)
	   AND (
	       sqlc.narg(status)::text IS NULL
	       OR CASE waitpoints.status
	           WHEN 'pending' THEN 'pending'
	           WHEN 'completed' THEN 'completed'
	           WHEN 'timed_out' THEN 'timed_out'
	           WHEN 'cancelled' THEN 'cancelled'
	           ELSE 'failed'
	       END = sqlc.narg(status)::text
	   )
	   AND (
	       sqlc.narg(after_id)::uuid IS NULL
       OR (waitpoints.created_at, waitpoints.id) > (
           SELECT cursor_wait.created_at, cursor_wait.id FROM cursor_wait
       )
   )
 ORDER BY waitpoints.created_at ASC, waitpoints.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: GetRunWaitpointCursor :one
SELECT waitpoints.created_at, waitpoints.id
  FROM waitpoints
 WHERE waitpoints.org_id = sqlc.arg(org_id)
   AND waitpoints.run_id = sqlc.arg(run_id)
   AND waitpoints.id = sqlc.arg(cursor_id)::uuid;
