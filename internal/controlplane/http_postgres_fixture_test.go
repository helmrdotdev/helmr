package controlplane

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"

	"github.com/jackc/pgx/v5/pgxpool"
)

// httpPostgresFixture serves the control plane built by NewServer over a test
// database, authenticating requests with real login sessions.
type httpPostgresFixture struct {
	pool                 *pgxpool.Pool
	queries              *db.Queries
	handler              http.Handler
	keys                 auth.Keys
	operatorTokens       map[string]bool
	browserOrganizations map[string]uuid.UUID
	origin               string
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
	return httpPostgresFixture{pool: database.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
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

// session creates a browser account proof. orgID is the test client's explicit
// request selection and is not stored in the browser session.
func (f httpPostgresFixture) session(t *testing.T, userID uuid.UUID, orgID uuid.UUID) string {
	t.Helper()
	token, err := auth.GenerateOpaque(32)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashToken(f.keys.Session, token)
	if err != nil {
		t.Fatal(err)
	}
	source, session := uuid.NewV7(), uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO auth_identities(id,user_id,provider,subject,last_authenticated_at) VALUES($1,$2,'fixture',$1::uuid::text,clock_timestamp())`, source, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO login_sessions(id,user_id,channel,access_epoch,token_hash,identity_id,authenticated_at,expires_at) SELECT $1,id,'browser',access_epoch,$3,$4,clock_timestamp(),clock_timestamp()+interval '1 hour' FROM users WHERE id=$2`, session, userID, hash, source); err != nil {
		t.Fatal(err)
	}
	f.browserOrganizations[token] = orgID
	return token
}

// request uses cookies for fixture browsers and bearer authorization for other tokens.
func (f httpPostgresFixture) request(t *testing.T, method string, path string, token string, body string, headers ...http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	f.authenticateRequest(request, token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, values := range headers {
		for key, items := range values {
			for _, value := range items {
				request.Header.Add(key, value)
			}
		}
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
	if issuer.Channel == "" {
		source, session := uuid.NewV7(), uuid.NewV7()
		dbtest.MustExec(t, ctx, database, `INSERT INTO auth_identities(id,user_id,provider,subject,last_authenticated_at) VALUES($1,$2,'fixture',$1::uuid::text,clock_timestamp())`, source, issuer.UserID)
		dbtest.MustExec(t, ctx, database, `INSERT INTO login_sessions(id,user_id,channel,access_epoch,token_hash,identity_id,authenticated_at,expires_at) SELECT $1,id,'browser',access_epoch,uuid_send($1::uuid),$3,clock_timestamp(),clock_timestamp()+interval '1 hour' FROM users WHERE id=$2`, session, issuer.UserID, source)
		var err error
		issuer, err = access.SessionPrincipal(ctx, database, session, scope.OrgID)
		if err != nil {
			return identity.IssuedAPIKey{}, err
		}
	}
	issued, err := identity.IssueAPIKey(ctx, database, auditpolicy.Policy{RetentionSeconds: 3600}, issuer, scope, identity.APIKeyInput{Name: input.Name})
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

func (f httpPostgresFixture) authenticateRequest(request *http.Request, token string) {
	request.Header.Set("Origin", f.origin)
	if token != "" {
		if f.operatorTokens[token] {
			request.AddCookie(&http.Cookie{Name: operatorCookieName(request), Value: token})
		} else if orgID, browser := f.browserOrganizations[token]; browser {
			request.AddCookie(&http.Cookie{Name: sessionCookieName(request), Value: token})
			if orgID != uuid.Nil() {
				request.Header.Set("X-Helmr-Organization", orgID.String())
			}
		} else {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
}

// cliSession exercises the consent/exchange route for bearer-only clients.
func (f httpPostgresFixture) cliSession(t *testing.T, user, organization uuid.UUID) string {
	t.Helper()
	cfg := identity.NewConfig(f.keys, identity.Lifetimes{}, auditpolicy.Policy{RetentionSeconds: 3600})
	raw := f.session(t, user, organization)
	p, err := identity.AuthenticateLoginSession(t.Context(), f.pool, cfg, raw, organization)
	if err != nil {
		t.Fatal(err)
	}
	device, err := identity.StartDeviceCode(t.Context(), f.pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = identity.ApproveDeviceCode(t.Context(), f.pool, cfg, p, identity.DeviceConsent{OrgID: organization.String()}, device.UserCode); err != nil {
		t.Fatal(err)
	}
	result, err := identity.ExchangeDeviceCode(t.Context(), f.pool, cfg, device.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
