-- name: EnsureOrgMember :one
INSERT INTO org_members (id,org_id,user_id,is_owner,display_name)
VALUES (sqlc.arg(id),sqlc.arg(org_id),sqlc.arg(user_id),sqlc.arg(is_owner),sqlc.arg(display_name))
ON CONFLICT (org_id,user_id) WHERE removed_at IS NULL DO UPDATE
SET display_name=EXCLUDED.display_name, updated_at=now()
RETURNING *;

-- name: ListOrgMembers :many
SELECT m.*, COALESCE(m.display_name,u.display_name)::text AS effective_display_name,u.primary_email
FROM org_members m JOIN users u ON u.id=m.user_id
WHERE m.org_id=$1
ORDER BY m.removed_at IS NULL DESC,m.created_at;

-- name: GetOrgMemberForManagement :one
SELECT m.*,u.primary_email
FROM org_members m JOIN users u ON u.id=m.user_id
WHERE m.org_id=$1 AND m.id=$2;

-- name: GetActiveOrgMemberByUser :one
SELECT * FROM org_members WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL;

-- name: UpdateOrgMemberOwnership :one
UPDATE org_members SET is_owner=sqlc.arg(is_owner),updated_at=now()
WHERE org_id=sqlc.arg(org_id) AND id=sqlc.arg(id)
AND removed_at IS NULL AND is_owner=sqlc.arg(expected_is_owner)
RETURNING *;

-- name: RemoveOrgMember :one
UPDATE org_members SET removed_at=now(),updated_at=now()
WHERE org_id=$1 AND id=$2 AND removed_at IS NULL AND NOT is_owner
RETURNING *;
