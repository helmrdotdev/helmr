package identity

import (
	"bytes"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type identityFixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	keys    auth.Keys
	cfg     Config
}

func newIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(bytes.Repeat([]byte{3}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return identityFixture{
		pool:    database.Pool,
		queries: db.New(database.Pool),
		keys:    keys,
		cfg:     NewConfig(keys, Lifetimes{}, []string{" Admin@Example.test "}),
	}
}

func (f identityFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f identityFixture) organization(t *testing.T, slug string) uuid.UUID {
	t.Helper()
	orgID := uuid.NewV7()
	if _, err := f.queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{ID: pgvalue.UUID(orgID), Name: slug, Slug: slug}); err != nil {
		t.Fatal(err)
	}
	return orgID
}

func (f identityFixture) user(t *testing.T, name string, email string) uuid.UUID {
	t.Helper()
	userID := uuid.NewV7()
	f.exec(t, `INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, NULLIF($3, ''))`, userID, name, email)
	return userID
}

func (f identityFixture) member(t *testing.T, orgID uuid.UUID, userID uuid.UUID, role db.OrgMemberRole) {
	t.Helper()
	if _, err := f.queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), Role: role}); err != nil {
		t.Fatal(err)
	}
}

// invitation creates an invitation from a new owner of the organization and
// returns its raw token.
func (f identityFixture) invitation(t *testing.T, orgID uuid.UUID, email string, role string) (db.Invitation, string) {
	t.Helper()
	owner := f.user(t, "Owner", "")
	f.member(t, orgID, owner, db.OrgMemberRoleOwner)
	invitation, rawToken, err := org.CreateInvitation(t.Context(), f.queries, f.keys.Invitation,
		org.ManagingMember{OrgID: orgID, UserID: owner, Role: auth.RoleOwner}, org.InvitationInput{Email: email, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return invitation, rawToken
}

func (f identityFixture) session(t *testing.T, userID uuid.UUID, orgID uuid.UUID) string {
	t.Helper()
	orgRef := pgtype.UUID{}
	if orgID != uuid.Nil() {
		orgRef = pgvalue.UUID(orgID)
	}
	raw, err := issueLoginSession(t.Context(), f.queries, f.cfg, pgvalue.UUID(userID), orgRef)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f identityFixture) activeSessions(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM auth_sessions WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f identityFixture) userByEmail(t *testing.T, email string) (uuid.UUID, bool) {
	t.Helper()
	var userID uuid.UUID
	var admin bool
	if err := f.pool.QueryRow(t.Context(), `SELECT id, admin FROM users WHERE lower(primary_email) = lower($1)`, email).Scan(&userID, &admin); err != nil {
		t.Fatal(err)
	}
	return userID, admin
}

func (f identityFixture) expire(t *testing.T, table string, id pgtype.UUID) {
	t.Helper()
	f.exec(t, `UPDATE `+table+` SET expires_at = $2 WHERE id = $1`, id, time.Now().Add(-time.Second))
}

// rejectLoginSessions makes every login session insert fail until the
// returned function restores it, so that a sign-in fails after its earlier
// writes.
func (f identityFixture) rejectLoginSessions(t *testing.T) func() {
	t.Helper()
	f.exec(t, `
		CREATE FUNCTION reject_login_session() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected login session failure';
		END
		$$`)
	f.exec(t, `CREATE TRIGGER reject_login_session BEFORE INSERT ON auth_sessions FOR EACH ROW EXECUTE FUNCTION reject_login_session()`)
	return func() {
		f.exec(t, `DROP TRIGGER reject_login_session ON auth_sessions`)
		f.exec(t, `DROP FUNCTION reject_login_session()`)
	}
}

func (f identityFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM auth_sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
