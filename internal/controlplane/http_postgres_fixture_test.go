package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// httpPostgresFixture serves the control plane built by NewServer over a test
// database, authenticating requests with real login sessions.
type httpPostgresFixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	handler http.Handler
	keys    auth.Keys
}

// newHTTPPostgresFixture builds the server from the complete test
// configuration over a fresh database; configure adjusts it before NewServer.
func newHTTPPostgresFixture(t *testing.T, configure ...func(*ServerConfig)) httpPostgresFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	cfg := completeServerConfig(t)
	cfg.DB = queries
	cfg.TX = database.Pool
	cfg.DiagnosticDB = database.Pool
	cfg.Auth = identity.NewAPIKeyAuthenticator(database.Pool)
	cfg.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}
	for _, apply := range configure {
		apply(&cfg)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return httpPostgresFixture{pool: database.Pool, queries: queries, handler: handler, keys: keys}
}

// organizationOwner creates an organization with an owner and returns the
// owner's session token for it.
func (f httpPostgresFixture) organizationOwner(t *testing.T, slug string) (uuid.UUID, string) {
	t.Helper()
	orgID := uuid.NewV7()
	if _, err := f.queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: slug, Slug: slug,
	}); err != nil {
		t.Fatal(err)
	}
	userID := f.user(t, "Owner")
	f.member(t, orgID, userID, "owner")
	return orgID, f.session(t, userID, orgID)
}

func (f httpPostgresFixture) user(t *testing.T, name string) uuid.UUID {
	t.Helper()
	userID := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, name); err != nil {
		t.Fatal(err)
	}
	return userID
}

func (f httpPostgresFixture) member(t *testing.T, orgID uuid.UUID, userID uuid.UUID, role string) uuid.UUID {
	t.Helper()
	record, err := f.queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{ID: pgvalue.NewUUIDv7(),
		OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), IsOwner: role == "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if role == "owner" || role == "admin" {
		dbtest.GrantMember(t, f.pool, orgID, userID, nil, "members.read", "members.remove", "invitations.read", "invitations.create", "invitations.revoke", "groups.read", "groups.create", "groups.update", "groups.delete", "groups.add_member", "groups.remove_member", "roles.read", "roles.create", "roles.update", "roles.delete", "assignments.read", "assignments.create", "assignments.update", "assignments.delete", "projects.read", "projects.create", "projects.update", "projects.update_retention", "environments.read", "environments.create", "environments.update")
	}
	return pgvalue.MustUUIDValue(record.ID)
}

// session creates a login session for the user that selects orgID, or no
// organization when orgID is nil, and returns its token.
func (f httpPostgresFixture) session(t *testing.T, userID uuid.UUID, orgID uuid.UUID) string {
	t.Helper()
	token, err := auth.GenerateOpaque(32)
	if err != nil {
		t.Fatal(err)
	}
	tokenHash, err := auth.HashToken(f.keys.Session, token)
	if err != nil {
		t.Fatal(err)
	}
	orgRef := pgtype.UUID{}
	if orgID != uuid.Nil() {
		orgRef = pgvalue.UUID(orgID)
	}
	if _, err := f.queries.CreateLoginSession(t.Context(), db.CreateLoginSessionParams{
		ID: pgvalue.UUID(uuid.NewV7()), OrgID: orgRef, UserID: pgvalue.UUID(userID),
		TokenHash: tokenHash, ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

// request serves a request authenticated by a bearer session token, or
// unauthenticated when token is empty.
func (f httpPostgresFixture) request(t *testing.T, method string, path string, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func (f httpPostgresFixture) grant(t *testing.T, org, user, env uuid.UUID, permission string) {
	t.Helper()
	dbtest.GrantMember(t, f.pool, org, user, &env, permission)
}

// testAPIKeyInput gives workload fixtures an explicit custom Role. Production
// issuance and role-assignment authorization are covered by their own tests.
type testAPIKeyInput struct {
	Name            string
	RolePermissions []auth.Permission
}

func issueAPIKeyWithRole(t *testing.T, ctx context.Context, database db.TxDB, issuer auth.Principal, scope auth.Scope, input testAPIKeyInput) (identity.IssuedAPIKey, error) {
	t.Helper()
	issued, err := identity.IssueAPIKey(ctx, database, issuer, scope, identity.APIKeyInput{Name: input.Name})
	if err != nil {
		return issued, err
	}
	values := make([]string, 0, len(input.RolePermissions))
	for _, p := range input.RolePermissions {
		values = append(values, string(p))
	}
	dbtest.SetAPIKeyRole(t, database, pgvalue.MustUUIDValue(issued.Record.ID), values...)
	return issued, nil
}
