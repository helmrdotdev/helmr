package sso

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultProofLifetime is how long a browser SSO proof admits its organization
// when the organization sets no lifetime.
const DefaultProofLifetime = 24 * time.Hour

var (
	ErrNotBound         = errors.New("this company identity is not linked to a member")
	ErrIdentityConflict = errors.New("this company identity or membership is already linked to another")
)

// Binding is a live link of one connection subject to one membership.
type Binding struct {
	ID         uuid.UUID
	OrgID      uuid.UUID
	IdentityID uuid.UUID
	MemberID   uuid.UUID
	UserID     uuid.UUID
}

// LockActive holds an active connection against retirement for the rest of the
// caller's transaction and returns its organization.
func LockActive(ctx context.Context, tx pgx.Tx, connection uuid.UUID) (uuid.UUID, error) {
	var org uuid.UUID
	err := tx.QueryRow(ctx, `SELECT org_id FROM org_sso_connections WHERE id=$1 AND state='active' FOR SHARE`, connection).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil(), ErrConnectionNotFound
	}
	return org, err
}

// LiveBinding finds the live binding of a connection subject without locking
// it. Member retirement locks the member before the binding, so a caller locks
// the member next and then holds the binding with HoldBinding.
func LiveBinding(ctx context.Context, q db.DBTX, connection uuid.UUID, subject string) (Binding, error) {
	var b Binding
	err := q.QueryRow(ctx, `SELECT b.id,b.org_id,b.identity_id,b.member_id,b.user_id FROM org_sso_identities i
 JOIN org_sso_member_bindings b ON b.identity_id=i.id AND b.retired_at IS NULL
 WHERE i.connection_id=$1 AND i.subject=$2`, connection, subject).Scan(&b.ID, &b.OrgID, &b.IdentityID, &b.MemberID, &b.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, ErrNotBound
	}
	return b, err
}

// HoldBinding holds a live binding against retirement for the rest of the
// caller's transaction.
func HoldBinding(ctx context.Context, tx pgx.Tx, b Binding) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM org_sso_member_bindings WHERE id=$1 AND retired_at IS NULL FOR SHARE`, b.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotBound
	}
	return err
}

// Link binds a connection subject to a membership of user. The subject belongs
// to one user forever; each identity and each member has one live binding. A
// live binding of the same pair is returned unchanged with created false.
func Link(ctx context.Context, tx pgx.Tx, org, connection uuid.UUID, subject string, member, user uuid.UUID) (Binding, bool, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO org_sso_identities(id,org_id,connection_id,subject,user_id) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT (connection_id,subject) DO NOTHING`, uuid.NewV7(), org, connection, subject, user); err != nil {
		return Binding{}, false, err
	}
	b := Binding{OrgID: org, MemberID: member, UserID: user}
	var owner uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id,user_id FROM org_sso_identities WHERE connection_id=$1 AND subject=$2 FOR UPDATE`, connection, subject).Scan(&b.IdentityID, &owner); err != nil {
		return Binding{}, false, err
	}
	if owner != user {
		return Binding{}, false, ErrIdentityConflict
	}
	rows, err := tx.Query(ctx, `SELECT id,identity_id,member_id FROM org_sso_member_bindings
 WHERE org_id=$1 AND retired_at IS NULL AND (identity_id=$2 OR member_id=$3)`, org, b.IdentityID, member)
	if err != nil {
		return Binding{}, false, err
	}
	var live []Binding
	for rows.Next() {
		var l Binding
		if err := rows.Scan(&l.ID, &l.IdentityID, &l.MemberID); err != nil {
			rows.Close()
			return Binding{}, false, err
		}
		live = append(live, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Binding{}, false, err
	}
	if len(live) == 1 && live[0].IdentityID == b.IdentityID && live[0].MemberID == member {
		b.ID = live[0].ID
		return b, false, nil
	}
	if len(live) > 0 {
		return Binding{}, false, ErrIdentityConflict
	}
	b.ID = uuid.NewV7()
	_, err = tx.Exec(ctx, `INSERT INTO org_sso_member_bindings(id,org_id,identity_id,member_id,user_id) VALUES($1,$2,$3,$4,$5)`, b.ID, org, b.IdentityID, member, user)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return Binding{}, false, ErrIdentityConflict
	}
	return b, err == nil, err
}

// ProofLifetime is how long a browser SSO proof admits org.
func ProofLifetime(ctx context.Context, q db.DBTX, org uuid.UUID) (time.Duration, error) {
	var seconds int64
	err := q.QueryRow(ctx, `SELECT COALESCE(sso_session_seconds,86400) FROM organizations WHERE id=$1`, org).Scan(&seconds)
	return time.Duration(seconds) * time.Second, err
}

// AddProof gives a browser session the binding's SSO proof, replacing any
// earlier proof the session held for that organization.
func AddProof(ctx context.Context, tx pgx.Tx, session uuid.UUID, b Binding, authenticatedAt time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE browser_sso_proofs SET revoked_at=clock_timestamp() WHERE session_id=$1 AND org_id=$2 AND revoked_at IS NULL`, session, b.OrgID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO browser_sso_proofs(id,session_id,org_id,member_id,user_id,binding_id,authenticated_at) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		uuid.NewV7(), session, b.OrgID, b.MemberID, b.UserID, b.ID, authenticatedAt)
	return err
}

// RetireMemberAccess runs when a membership ends: its binding retires and the
// browser SSO proofs derived from it are revoked.
func RetireMemberAccess(ctx context.Context, tx pgx.Tx, org, member uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE org_sso_member_bindings SET retired_at=clock_timestamp() WHERE org_id=$1 AND member_id=$2 AND retired_at IS NULL`, org, member); err != nil {
		return err
	}
	return RevokeMemberProofs(ctx, tx, org, member)
}

// RevokeMemberProofs revokes a member's browser SSO proofs and keeps the
// binding, for suspension.
func RevokeMemberProofs(ctx context.Context, tx pgx.Tx, org, member uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE browser_sso_proofs SET revoked_at=clock_timestamp() WHERE org_id=$1 AND member_id=$2 AND revoked_at IS NULL`, org, member)
	return err
}

// RevokeAccountProofs revokes the browser SSO proofs of every session of user
// except keep. A zero keep revokes them all.
func RevokeAccountProofs(ctx context.Context, tx pgx.Tx, user, keep uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE browser_sso_proofs SET revoked_at=clock_timestamp() WHERE user_id=$1 AND session_id<>$2 AND revoked_at IS NULL`, user, keep)
	return err
}

// AccountBinding is one organization in which an account is linked to company SSO.
type AccountBinding struct {
	OrgID    uuid.UUID
	OrgName  string
	Protocol string
	Subject  string
	LinkedAt time.Time
}

// AccountBindings lists the account's live bindings through active connections.
func AccountBindings(ctx context.Context, q db.DBTX, user uuid.UUID) ([]AccountBinding, error) {
	rows, err := q.Query(ctx, `SELECT b.org_id,o.name,c.protocol,i.subject,b.created_at FROM org_sso_member_bindings b
 JOIN organizations o ON o.id=b.org_id
 JOIN org_sso_identities i ON i.id=b.identity_id
 JOIN org_sso_connections c ON c.id=i.connection_id AND c.state='active'
 WHERE b.user_id=$1 AND b.retired_at IS NULL ORDER BY b.created_at,b.id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountBinding
	for rows.Next() {
		var b AccountBinding
		if err := rows.Scan(&b.OrgID, &b.OrgName, &b.Protocol, &b.Subject, &b.LinkedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
