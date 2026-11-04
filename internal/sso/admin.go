package sso

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var (
	ErrUntested          = errors.New("test the connection before activating it")
	ErrEnforcerNotLinked = errors.New("the enforcing owner is not signed in through a linked company account")
)

// LinkProof reports whether proof is a live browser SSO proof.
func LinkProof(ctx context.Context, q db.DBTX, proof uuid.UUID) (bool, error) {
	var linked bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sso_proofs WHERE id=$1 AND revoked_at IS NULL)`, proof).Scan(&linked)
	return linked, err
}

// Candidate reads an organization's candidate connection without locking it.
func (s *Store) Candidate(ctx context.Context, q db.DBTX, org uuid.UUID) (Connection, error) {
	return s.scan(q.QueryRow(ctx, `SELECT id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_client_secret_ciphertext,oidc_client_secret_nonce,
 saml_entity_id,saml_sso_url,saml_certificates,tested_at,created_at,activated_at
 FROM sso_connections WHERE org_id=$1 AND state='candidate'`, org))
}

// Activate makes a tested candidate the active connection. The caller retires
// any previous active connection first.
func Activate(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) error {
	var tested *time.Time
	err := tx.QueryRow(ctx, `SELECT tested_at FROM sso_connections WHERE org_id=$1 AND id=$2 AND state='candidate' FOR UPDATE`, org, id).Scan(&tested)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConnectionNotFound
	}
	if err != nil {
		return err
	}
	if tested == nil {
		return ErrUntested
	}
	return affected(tx.Exec(ctx, `UPDATE sso_connections SET state='active',activated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND state='candidate'`, org, id))
}

// RetireActive retires an organization's active connection and its live
// links, returning the retired connection and link IDs. It retires
// nothing when no connection is active.
func RetireActive(ctx context.Context, tx pgx.Tx, org uuid.UUID) (Connection, []uuid.UUID, error) {
	var c Connection
	err := tx.QueryRow(ctx, `UPDATE sso_connections SET state='retired',retired_at=clock_timestamp() WHERE org_id=$1 AND state='active' RETURNING id,protocol`, org).Scan(&c.ID, &c.Protocol)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, nil, nil
	}
	if err != nil {
		return Connection{}, nil, err
	}
	links, err := collectIDs(tx.Query(ctx, `UPDATE sso_member_links b SET retired_at=clock_timestamp() FROM sso_identities i
 WHERE i.id=b.identity_id AND i.connection_id=$1 AND b.retired_at IS NULL RETURNING b.id`, c.ID))
	return c, links, err
}

// RetiredLink is a link just retired, with the identity it linked.
type RetiredLink struct {
	ID           uuid.UUID
	IdentityID   uuid.UUID
	ConnectionID uuid.UUID
	Subject      string
}

// RetireMemberLink retires one member's live link. The result is zero
// when the member has none.
func RetireMemberLink(ctx context.Context, tx pgx.Tx, org, member uuid.UUID) (RetiredLink, error) {
	var r RetiredLink
	err := tx.QueryRow(ctx, `UPDATE sso_member_links b SET retired_at=clock_timestamp() FROM sso_identities i
 WHERE i.id=b.identity_id AND b.org_id=$1 AND b.member_id=$2 AND b.retired_at IS NULL RETURNING b.id,i.id,i.connection_id,i.subject`, org, member).Scan(&r.ID, &r.IdentityID, &r.ConnectionID, &r.Subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetiredLink{}, nil
	}
	return r, err
}

// RevokeLinkProofs revokes the live browser SSO proofs of the links.
func RevokeLinkProofs(ctx context.Context, tx pgx.Tx, links []uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE sso_proofs SET revoked_at=clock_timestamp() WHERE link_id=ANY($1) AND revoked_at IS NULL`, links)
	return err
}

// LinkedMember is one member and their live link, if any.
type LinkedMember struct {
	MemberID    uuid.UUID
	UserID      uuid.UUID
	DisplayName string
	Email       string
	Subject     string
	LinkedAt    *time.Time
}

// LinkedMembers lists the organization's live members with their link
// through the active connection.
func LinkedMembers(ctx context.Context, q db.DBTX, org uuid.UUID) ([]LinkedMember, error) {
	rows, err := q.Query(ctx, `SELECT m.id,m.user_id,COALESCE(m.display_name,u.display_name),COALESCE(u.primary_email,''),COALESCE(i.subject,''),b.created_at
 FROM org_members m JOIN users u ON u.id=m.user_id
 LEFT JOIN sso_member_links b ON b.member_id=m.id AND b.retired_at IS NULL
 LEFT JOIN sso_identities i ON i.id=b.identity_id
 WHERE m.org_id=$1 AND m.removed_at IS NULL ORDER BY m.created_at,m.id`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LinkedMember
	for rows.Next() {
		var m LinkedMember
		if err := rows.Scan(&m.MemberID, &m.UserID, &m.DisplayName, &m.Email, &m.Subject, &m.LinkedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func collectIDs(rows pgx.Rows, err error) ([]uuid.UUID, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
