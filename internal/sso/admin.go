package sso

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var (
	ErrUntested         = errors.New("test the connection before activating it")
	ErrNoRecoveryCodes  = errors.New("generate recovery codes before requiring single sign-on")
	ErrRecoveryCodeUsed = errors.New("the recovery code is invalid or already used")
)

// Fingerprint identifies a connection's current trust material. A test sign-in
// marks a connection tested only if its material did not change meanwhile.
func (c Connection) Fingerprint() []byte {
	h := sha256.New()
	for _, part := range []string{c.Protocol, c.OIDC.Issuer, c.OIDC.ClientID, c.OIDC.ClientSecret, c.SAML.EntityID, c.SAML.SSOURL, strings.Join(c.SAML.Certificates, "\x00")} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// Candidate reads an organization's candidate connection without locking it.
func (s *Store) Candidate(ctx context.Context, q db.DBTX, org uuid.UUID) (Connection, error) {
	return s.scan(q.QueryRow(ctx, `SELECT id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_secret_ciphertext,oidc_secret_nonce,
 saml_idp_entity_id,saml_sso_url,saml_certificates,tested_at,created_at,activated_at
 FROM org_sso_connections WHERE org_id=$1 AND state='candidate'`, org))
}

// Activate makes a tested candidate the active connection. The caller retires
// any previous active connection first.
func Activate(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) error {
	var tested *time.Time
	err := tx.QueryRow(ctx, `SELECT tested_at FROM org_sso_connections WHERE org_id=$1 AND id=$2 AND state='candidate' FOR UPDATE`, org, id).Scan(&tested)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConnectionNotFound
	}
	if err != nil {
		return err
	}
	if tested == nil {
		return ErrUntested
	}
	return affected(tx.Exec(ctx, `UPDATE org_sso_connections SET state='active',activated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND state='candidate'`, org, id))
}

// RetireActive retires an organization's active connection and its live
// bindings, returning the retired connection and binding IDs. It retires
// nothing when no connection is active.
func RetireActive(ctx context.Context, tx pgx.Tx, org uuid.UUID) (Connection, []uuid.UUID, error) {
	var c Connection
	err := tx.QueryRow(ctx, `UPDATE org_sso_connections SET state='retired',retired_at=clock_timestamp() WHERE org_id=$1 AND state='active' RETURNING id,protocol`, org).Scan(&c.ID, &c.Protocol)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, nil, nil
	}
	if err != nil {
		return Connection{}, nil, err
	}
	bindings, err := collectIDs(tx.Query(ctx, `UPDATE org_sso_member_bindings b SET retired_at=clock_timestamp() FROM org_sso_identities i
 WHERE i.id=b.identity_id AND i.connection_id=$1 AND b.retired_at IS NULL RETURNING b.id`, c.ID))
	return c, bindings, err
}

// RetiredBinding is a binding just retired, with the identity it linked.
type RetiredBinding struct {
	ID           uuid.UUID
	IdentityID   uuid.UUID
	ConnectionID uuid.UUID
	Subject      string
}

// RetireMemberBinding retires one member's live binding. The result is zero
// when the member has none.
func RetireMemberBinding(ctx context.Context, tx pgx.Tx, org, member uuid.UUID) (RetiredBinding, error) {
	var r RetiredBinding
	err := tx.QueryRow(ctx, `UPDATE org_sso_member_bindings b SET retired_at=clock_timestamp() FROM org_sso_identities i
 WHERE i.id=b.identity_id AND b.org_id=$1 AND b.member_id=$2 AND b.retired_at IS NULL RETURNING b.id,i.id,i.connection_id,i.subject`, org, member).Scan(&r.ID, &r.IdentityID, &r.ConnectionID, &r.Subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetiredBinding{}, nil
	}
	return r, err
}

// RevokeBindingProofs revokes the live browser SSO proofs of the bindings.
func RevokeBindingProofs(ctx context.Context, tx pgx.Tx, bindings []uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE browser_sso_proofs SET revoked_at=clock_timestamp() WHERE binding_id=ANY($1) AND revoked_at IS NULL`, bindings)
	return err
}

// LinkedMember is one member and their live binding, if any.
type LinkedMember struct {
	MemberID    uuid.UUID
	UserID      uuid.UUID
	DisplayName string
	Email       string
	Subject     string
	LinkedAt    *time.Time
}

// LinkedMembers lists the organization's live members with their binding
// through the active connection.
func LinkedMembers(ctx context.Context, q db.DBTX, org uuid.UUID) ([]LinkedMember, error) {
	rows, err := q.Query(ctx, `SELECT m.id,m.user_id,COALESCE(m.display_name,u.display_name),COALESCE(u.primary_email,''),COALESCE(i.subject,''),b.created_at
 FROM org_members m JOIN users u ON u.id=m.user_id
 LEFT JOIN org_sso_member_bindings b ON b.member_id=m.id AND b.retired_at IS NULL
 LEFT JOIN org_sso_identities i ON i.id=b.identity_id
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

// ReplaceRecoveryCodes revokes the organization's live recovery codes and
// stores the new set's hashes.
func ReplaceRecoveryCodes(ctx context.Context, tx pgx.Tx, org, creator uuid.UUID, hashes [][]byte) error {
	if _, err := tx.Exec(ctx, `UPDATE org_recovery_codes SET revoked_at=clock_timestamp() WHERE org_id=$1 AND used_at IS NULL AND revoked_at IS NULL`, org); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `INSERT INTO org_recovery_codes(id,org_id,code_hash,created_by_member_id) VALUES($1,$2,$3,$4)`, uuid.NewV7(), org, h, creator); err != nil {
			return err
		}
	}
	return nil
}

// RecoveryCodes reports how many of the live set remain and when it was made.
func RecoveryCodes(ctx context.Context, q db.DBTX, org uuid.UUID) (int, *time.Time, error) {
	var remaining int
	var generated *time.Time
	err := q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE used_at IS NULL),max(created_at) FROM org_recovery_codes WHERE org_id=$1 AND revoked_at IS NULL`, org).Scan(&remaining, &generated)
	if remaining == 0 {
		generated = nil
	}
	return remaining, generated, err
}

// UseRecoveryCode consumes one live code of the organization and gives the
// browser session a proof from it.
func UseRecoveryCode(ctx context.Context, tx pgx.Tx, org, member, user, session uuid.UUID, hash []byte) (uuid.UUID, error) {
	var code uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE org_recovery_codes SET used_at=clock_timestamp(),used_by_member_id=$3
 WHERE org_id=$1 AND code_hash=$2 AND used_at IS NULL AND revoked_at IS NULL RETURNING id`, org, hash, member).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil(), ErrRecoveryCodeUsed
	}
	if err != nil {
		return uuid.Nil(), err
	}
	if _, err := tx.Exec(ctx, `UPDATE browser_sso_proofs SET revoked_at=clock_timestamp() WHERE session_id=$1 AND org_id=$2 AND revoked_at IS NULL`, session, org); err != nil {
		return uuid.Nil(), err
	}
	_, err = tx.Exec(ctx, `INSERT INTO browser_sso_proofs(id,session_id,org_id,member_id,user_id,recovery_code_id,authenticated_at) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp())`,
		uuid.NewV7(), session, org, member, user, code)
	return code, err
}

func collectIDs(rows pgx.Rows, err error) ([]uuid.UUID, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
