package controlplane

import (
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
	cfg.Auth = identity.NewAPIKeyAuthenticator(queries)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
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
	f.member(t, orgID, userID, db.OrgMemberRoleOwner)
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

func (f httpPostgresFixture) member(t *testing.T, orgID uuid.UUID, userID uuid.UUID, role db.OrgMemberRole) {
	t.Helper()
	if _, err := f.queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{
		OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), Role: role,
	}); err != nil {
		t.Fatal(err)
	}
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
	if _, err := f.queries.CreateAuthSession(t.Context(), db.CreateAuthSessionParams{
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
