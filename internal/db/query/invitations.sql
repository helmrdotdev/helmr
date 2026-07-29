-- name: GetActiveInvitation :one
SELECT id, org_id, invitee_email, role
  FROM invitations
 WHERE token_hash = sqlc.arg(token_hash)
   AND accepted_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now();

-- name: GetActiveInvitationByID :one
SELECT id, org_id, invitee_email, role
  FROM invitations
 WHERE id = sqlc.arg(id)
   AND accepted_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now();

-- name: ListInvitations :many
SELECT id,
       org_id,
       invitee_email,
       role,
       invited_by_user_id,
       created_at,
       expires_at,
       accepted_at,
       accepted_by_user_id,
       revoked_at,
       revoked_by_user_id
  FROM invitations
 WHERE org_id = sqlc.arg(org_id)
   AND accepted_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now()
 ORDER BY created_at DESC
 LIMIT sqlc.arg(row_limit);

-- name: GetPendingInvitationByEmail :one
SELECT id, org_id, invitee_email, role
  FROM invitations
 WHERE org_id = sqlc.arg(org_id)
   AND invitee_email = sqlc.arg(invitee_email)
   AND accepted_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now()
 LIMIT 1;

-- name: GetRevocableInvitation :one
SELECT id, org_id, invitee_email, role
  FROM invitations
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND accepted_at IS NULL
   AND revoked_at IS NULL;

-- name: RevokeExpiredInvitationsByEmail :execrows
UPDATE invitations
   SET revoked_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND invitee_email = sqlc.arg(invitee_email)
   AND accepted_at IS NULL
   AND revoked_at IS NULL
   AND expires_at <= now();

-- name: CreateInvitation :one
WITH active_invitee AS (
    SELECT 1
      FROM org_members
      JOIN users ON users.id = org_members.user_id
     WHERE org_members.org_id = sqlc.arg(org_id)
       AND lower(users.primary_email) = sqlc.arg(invitee_email)
       AND org_members.disabled_at IS NULL
       AND users.disabled_at IS NULL
    UNION
    SELECT 1
      FROM invitations AS accepted_invitation
      JOIN org_members
        ON org_members.org_id = accepted_invitation.org_id
       AND org_members.user_id = accepted_invitation.accepted_by_user_id
      JOIN users ON users.id = org_members.user_id
     WHERE accepted_invitation.org_id = sqlc.arg(org_id)
       AND accepted_invitation.invitee_email = sqlc.arg(invitee_email)
       AND accepted_invitation.accepted_at IS NOT NULL
       AND org_members.disabled_at IS NULL
       AND users.disabled_at IS NULL
)
INSERT INTO invitations (id, org_id, invitee_email, role, invited_by_user_id, token_hash, expires_at)
SELECT
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(invitee_email),
    sqlc.arg(role)::org_member_role,
    sqlc.arg(invited_by_user_id),
    sqlc.arg(token_hash),
    sqlc.arg(expires_at)
WHERE NOT EXISTS (SELECT 1 FROM active_invitee)
RETURNING *;

-- name: AcceptInvitation :execrows
UPDATE invitations
   SET accepted_at = now(),
       accepted_by_user_id = sqlc.arg(user_id)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND accepted_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now();

-- name: RevokeInvitation :execrows
UPDATE invitations
   SET revoked_at = now(),
       revoked_by_user_id = sqlc.arg(revoked_by_user_id)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND accepted_at IS NULL
   AND revoked_at IS NULL;
