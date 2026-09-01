package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAPIKeyPermissionsPostgres(t *testing.T) {
	fixture := newAPIKeyPermissionPostgresFixture(t, 201)

	fixture.store.reset()
	first := fixture.list(t, "")
	if len(first.APIKeys) != apiKeyListLimit || first.NextCursor == "" {
		t.Fatalf("first list items/cursor = %d/%t", len(first.APIKeys), first.NextCursor != "")
	}
	if got := fixture.store.statements.Load(); got != 1 {
		t.Fatalf("first list statements = %d, want 1", got)
	}
	wantGrants := apiKeyPermissionGrantsFromPermissions(allAPIKeyInternalPermissions())
	for _, item := range first.APIKeys {
		if item.Name == "other-org" {
			t.Fatal("cross-organization API key appeared in target list")
		}
		itemWant := wantGrants
		if item.Name == "replacement" {
			itemWant = apiKeyPermissionGrantsFromPermissions(allAPIKeyInternalPermissions()[:1])
		}
		if !reflect.DeepEqual(item.Permissions, itemWant) {
			t.Fatalf("list permissions for %q = %+v, want %+v", item.Name, item.Permissions, itemWant)
		}
	}

	fixture.store.reset()
	second := fixture.list(t, first.NextCursor)
	if len(second.APIKeys) != 2 || second.NextCursor != "" || fixture.store.statements.Load() != 1 {
		t.Fatalf("second list items/cursor/statements = %d/%t/%d, want 2/false/1", len(second.APIKeys), second.NextCursor != "", fixture.store.statements.Load())
	}

	cursor, err := decodeAPIKeyListCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	cursor.ProjectID = uuid.NewV7().String()
	crossScope, err := encodeAPIKeyListCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.reset()
	if response := fixture.listRecorder(t, "active", crossScope); response.Code != http.StatusBadRequest || fixture.store.statements.Load() != 0 {
		t.Fatalf("cross-scope cursor status/statements = %d/%d, want 400/0", response.Code, fixture.store.statements.Load())
	}
	fixture.store.reset()
	if response := fixture.listRecorder(t, "active", "not-a-cursor"); response.Code != http.StatusBadRequest || fixture.store.statements.Load() != 0 {
		t.Fatalf("malformed cursor status/statements = %d/%d, want 400/0", response.Code, fixture.store.statements.Load())
	}

	invalidPermissions := []struct {
		name   string
		grants []api.APIKeyPermissionGrant
	}{
		{name: "empty"},
		{name: "empty grant", grants: []api.APIKeyPermissionGrant{{}}},
		{name: "unsupported", grants: []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{"unsupported"}}}},
	}
	for _, test := range invalidPermissions {
		fixture.store.reset()
		response := fixture.issueRecorder(t, test.name, test.grants)
		if response.Code != http.StatusBadRequest || fixture.store.statements.Load() != 0 {
			t.Fatalf("%s permissions status/statements = %d/%d, want 400/0", test.name, response.Code, fixture.store.statements.Load())
		}
	}

	inputScopes := append([]api.APIKeyScope{api.APIKeyScopeRunsRead}, allAPIKeyPermissionScopes()...)
	fixture.store.reset()
	issuedRecorder := fixture.issueRecorder(t, "issued", []api.APIKeyPermissionGrant{{Scopes: inputScopes}, {Scopes: []api.APIKeyScope{api.APIKeyScopeRunsRead}}})
	if issuedRecorder.Code != http.StatusCreated || fixture.store.statements.Load() != 1 {
		t.Fatalf("issue status/statements = %d/%d: %s", issuedRecorder.Code, fixture.store.statements.Load(), issuedRecorder.Body.String())
	}
	var issued api.APIKeyIssued
	if err := json.Unmarshal(issuedRecorder.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(issued.Permissions, wantGrants) {
		t.Fatalf("issued permissions = %+v, want %+v", issued.Permissions, wantGrants)
	}
	fixture.store.reset()
	principal, err := (dbAuthenticator{db: fixture.store}).Authenticate(t.Context(), issued.RawKey)
	if err != nil || fixture.store.statements.Load() != 1 || len(principal.Permissions) != len(allAPIKeyPermissionScopes()) {
		t.Fatalf("authenticate error/statements/permissions = %v/%d/%d", err, fixture.store.statements.Load(), len(principal.Permissions))
	}
	var touchedAt *time.Time
	if err := fixture.pool.QueryRow(t.Context(), `SELECT last_used_at FROM api_keys WHERE id = $1`, issued.ID).Scan(&touchedAt); err != nil || touchedAt == nil {
		t.Fatalf("authenticated last_used_at = %v, error = %v", touchedAt, err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, issued.ID); err != nil {
		t.Fatal(err)
	}
	fixture.store.reset()
	if _, err := (dbAuthenticator{db: fixture.store}).Authenticate(t.Context(), issued.RawKey); !errors.Is(err, auth.ErrUnauthenticated) || fixture.store.statements.Load() != 1 {
		t.Fatalf("revoked authenticate error/statements = %v/%d", err, fixture.store.statements.Load())
	}
	var revokedTouchedAt *time.Time
	if err := fixture.pool.QueryRow(t.Context(), `SELECT last_used_at FROM api_keys WHERE id = $1`, issued.ID).Scan(&revokedTouchedAt); err != nil || revokedTouchedAt == nil || !revokedTouchedAt.Equal(*touchedAt) {
		t.Fatalf("revoked last_used_at = %v, want %v, error = %v", revokedTouchedAt, touchedAt, err)
	}

	fixture.store.reset()
	expiredRecorder := fixture.issueRecorder(t, "expired", []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{api.APIKeyScopeRunsRead}}})
	if expiredRecorder.Code != http.StatusCreated || fixture.store.statements.Load() != 1 {
		t.Fatalf("expired issue status/statements = %d/%d: %s", expiredRecorder.Code, fixture.store.statements.Load(), expiredRecorder.Body.String())
	}
	var expired api.APIKeyIssued
	if err := json.Unmarshal(expiredRecorder.Body.Bytes(), &expired); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	fixture.store.reset()
	if _, err := (dbAuthenticator{db: fixture.store}).Authenticate(t.Context(), expired.RawKey); !errors.Is(err, auth.ErrUnauthenticated) || fixture.store.statements.Load() != 1 {
		t.Fatalf("expired authenticate error/statements = %v/%d", err, fixture.store.statements.Load())
	}
	var expiredUntouched bool
	if err := fixture.pool.QueryRow(t.Context(), `SELECT last_used_at IS NULL FROM api_keys WHERE id = $1`, expired.ID).Scan(&expiredUntouched); err != nil || !expiredUntouched {
		t.Fatalf("expired last_used_at untouched = %t, error = %v", expiredUntouched, err)
	}

	for _, status := range []struct {
		filter string
		name   string
	}{
		{filter: "revoked", name: "issued"},
		{filter: "expired", name: "expired"},
		{filter: "all", name: "issued"},
		{filter: "all", name: "expired"},
	} {
		fixture.store.reset()
		response := fixture.listWithFilter(t, status.filter, "")
		if fixture.store.statements.Load() != 1 || !containsAPIKeyNamed(response.APIKeys, status.name) || containsAPIKeyNamed(response.APIKeys, "other-org") {
			t.Fatalf("%s list statements/%q/other-org = %d/%t/%t", status.filter, status.name, fixture.store.statements.Load(), containsAPIKeyNamed(response.APIKeys, status.name), containsAPIKeyNamed(response.APIKeys, "other-org"))
		}
	}

	fixture.store.reset()
	replacementSuccess := fixture.issueRecorder(t, "replacement", []api.APIKeyPermissionGrant{{Scopes: allAPIKeyPermissionScopes()}})
	if replacementSuccess.Code != http.StatusCreated || fixture.store.statements.Load() != 1 {
		t.Fatalf("replacement success status/statements = %d/%d: %s", replacementSuccess.Code, fixture.store.statements.Load(), replacementSuccess.Body.String())
	}
	var activeReplacement api.APIKeyIssued
	if err := json.Unmarshal(replacementSuccess.Body.Bytes(), &activeReplacement); err != nil {
		t.Fatal(err)
	}
	activeReplacementID := uuid.MustParse(activeReplacement.ID)
	var oldRevoked bool
	if err := fixture.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1`, fixture.replacementID).Scan(&oldRevoked); err != nil {
		t.Fatal(err)
	}
	if !oldRevoked || !reflect.DeepEqual(activeReplacement.Permissions, wantGrants) {
		t.Fatalf("successful replacement old_revoked/permissions = %t/%+v", oldRevoked, activeReplacement.Permissions)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		CREATE FUNCTION reject_replacement_api_key() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.name = 'replacement' THEN
				RAISE EXCEPTION 'injected replacement failure';
			END IF;
			RETURN NEW;
		END
		$$
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `CREATE TRIGGER reject_replacement_api_key BEFORE INSERT ON api_keys FOR EACH ROW EXECUTE FUNCTION reject_replacement_api_key()`); err != nil {
		t.Fatal(err)
	}
	fixture.store.reset()
	failure := fixture.issueRecorder(t, "replacement", []api.APIKeyPermissionGrant{{Scopes: allAPIKeyPermissionScopes()}})
	if failure.Code != http.StatusInternalServerError || fixture.store.statements.Load() != 1 || bytes.Contains(failure.Body.Bytes(), []byte(`"raw_key"`)) {
		t.Fatalf("replacement failure status/statements/body = %d/%d/%s", failure.Code, fixture.store.statements.Load(), failure.Body.String())
	}
	var activeID uuid.UUID
	var activePermissions []string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT id, permissions
		  FROM api_keys
		 WHERE org_id = $1 AND project_id = $2 AND environment_id = $3 AND name = 'replacement' AND revoked_at IS NULL
	`, fixture.orgID, fixture.projectID, fixture.environmentID).Scan(&activeID, &activePermissions); err != nil {
		t.Fatal(err)
	}
	if activeID != activeReplacementID || !reflect.DeepEqual(activePermissions, allAPIKeyInternalPermissions()) {
		t.Fatalf("active replacement id/permissions = %s/%v", activeID, activePermissions)
	}
}

type apiKeyPermissionPostgresFixture struct {
	pool          *pgxpool.Pool
	store         *apiKeyPermissionCountingStore
	server        *Server
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	userID        uuid.UUID
	replacementID uuid.UUID
}

func newAPIKeyPermissionPostgresFixture(t *testing.T, keyCount int) apiKeyPermissionPostgresFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	orgID, projectID, environmentID, userID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	otherOrgID, otherProjectID, otherEnvironmentID, otherUserID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	setup := []struct {
		query string
		args  []any
	}{
		{query: `INSERT INTO regions (id, display_name) VALUES ('api-key-baseline', 'API key baseline')`},
		{query: `INSERT INTO organizations (id, name, slug) VALUES ($1, 'API key baseline', $2)`, args: []any{orgID, "api-key-baseline-" + orgID.String()}},
		{query: `INSERT INTO users (id, display_name, primary_email) VALUES ($1, 'API key baseline', $2)`, args: []any{userID, orgID.String() + "@example.test"}},
		{query: `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, args: []any{orgID, userID}},
		{query: `INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default) VALUES ($1, $2, 'api-key-baseline', 'api-key-baseline', 'API key baseline', true)`, args: []any{projectID, orgID}},
		{query: `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default) VALUES ($1, $2, $3, 'production', 'Production', '#315FCE', true)`, args: []any{environmentID, orgID, projectID}},
		{query: `INSERT INTO organizations (id, name, slug) VALUES ($1, 'Other organization', $2)`, args: []any{otherOrgID, "other-org-" + otherOrgID.String()}},
		{query: `INSERT INTO users (id, display_name, primary_email) VALUES ($1, 'Other organization', $2)`, args: []any{otherUserID, otherOrgID.String() + "@example.test"}},
		{query: `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, args: []any{otherOrgID, otherUserID}},
		{query: `INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default) VALUES ($1, $2, 'api-key-baseline', 'other', 'Other', true)`, args: []any{otherProjectID, otherOrgID}},
		{query: `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default) VALUES ($1, $2, $3, 'production', 'Production', '#315FCE', true)`, args: []any{otherEnvironmentID, otherOrgID, otherProjectID}},
	}
	for _, statement := range setup {
		if _, err := database.Pool.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	permissions := allAPIKeyInternalPermissions()
	keyRows := make([][]any, 0, keyCount+1)
	for index := range keyCount {
		keyID := uuid.NewV7()
		keyRows = append(keyRows, []any{keyID, orgID, projectID, environmentID, userID, "owner", permissions, fmt.Sprintf("key-%03d", index), fmt.Sprintf("hlmr_%03d", index), []byte(fmt.Sprintf("hash-%03d", index))})
	}
	replacementID := uuid.NewV7()
	keyRows = append(keyRows, []any{replacementID, orgID, projectID, environmentID, userID, "owner", []string{permissions[0]}, "replacement", "hlmr_replacement", []byte("replacement-old-hash")})
	keyRows = append(keyRows, []any{uuid.NewV7(), otherOrgID, otherProjectID, otherEnvironmentID, otherUserID, "owner", permissions, "other-org", "hlmr_other_org", []byte("other-org-hash")})
	if _, err := database.Pool.CopyFrom(t.Context(), pgx.Identifier{"api_keys"},
		[]string{"id", "org_id", "project_id", "environment_id", "created_by_user_id", "role", "permissions", "name", "key_prefix", "token_hash"}, pgx.CopyFromRows(keyRows)); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	store := &apiKeyPermissionCountingStore{Querier: queries}
	return apiKeyPermissionPostgresFixture{
		pool: database.Pool, store: store, server: &Server{db: store},
		orgID: orgID, projectID: projectID, environmentID: environmentID, userID: userID,
		replacementID: replacementID,
	}
}

func (f apiKeyPermissionPostgresFixture) list(t *testing.T, cursor string) api.ListAPIKeysResponse {
	t.Helper()
	return f.listWithFilter(t, "active", cursor)
}

func (f apiKeyPermissionPostgresFixture) listWithFilter(t *testing.T, filter, cursor string) api.ListAPIKeysResponse {
	t.Helper()
	recorder := f.listRecorder(t, filter, cursor)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response api.ListAPIKeysResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func (f apiKeyPermissionPostgresFixture) listRecorder(t *testing.T, filter, cursor string) *httptest.ResponseRecorder {
	t.Helper()
	path := fmt.Sprintf("/api/projects/%s/environments/%s/api-keys?filter=%s", f.projectID, f.environmentID, filter)
	if cursor != "" {
		path += "&cursor=" + cursor
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, auth.Actor{OrgID: f.orgID, UserID: f.userID, Kind: auth.ActorKindSession, Role: auth.RoleOwner}))
	recorder := httptest.NewRecorder()
	router := chi.NewRouter()
	router.Get("/api/projects/{projectID}/environments/{environmentID}/api-keys", f.server.listAPIKeys)
	router.ServeHTTP(recorder, request)
	return recorder
}

func containsAPIKeyNamed(keys []api.APIKeySummary, name string) bool {
	for _, key := range keys {
		if key.Name == name {
			return true
		}
	}
	return false
}

func (f apiKeyPermissionPostgresFixture) issueRecorder(t *testing.T, name string, grants []api.APIKeyPermissionGrant) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(api.IssueAPIKeyRequest{Name: name, Permissions: grants})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/environments/%s/api-keys", f.projectID, f.environmentID), bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, auth.Actor{OrgID: f.orgID, UserID: f.userID, Kind: auth.ActorKindSession, Role: auth.RoleOwner}))
	recorder := httptest.NewRecorder()
	router := chi.NewRouter()
	router.Post("/api/projects/{projectID}/environments/{environmentID}/api-keys", f.server.issueAPIKey)
	router.ServeHTTP(recorder, request)
	return recorder
}

func allAPIKeyPermissionScopes() []api.APIKeyScope {
	return []api.APIKeyScope{
		api.APIKeyScopeRunsCreate, api.APIKeyScopeRunsRead, api.APIKeyScopeRunsManage,
		api.APIKeyScopeSessionsRead, api.APIKeyScopeActorsStart, api.APIKeyScopeSessionsInputSend,
		api.APIKeyScopeSessionsClose, api.APIKeyScopeTokensCreate, api.APIKeyScopeTokensRead,
		api.APIKeyScopeTokensComplete, api.APIKeyScopeTokensCancel, api.APIKeyScopeWorkspacesCreate,
		api.APIKeyScopeWorkspacesRead, api.APIKeyScopeWorkspacesDelete, api.APIKeyScopeWorkspaceFilesRead,
		api.APIKeyScopeWorkspaceExecCreate, api.APIKeyScopeSecretsWrite, api.APIKeyScopeTasksDeploy,
	}
}

func allAPIKeyInternalPermissions() []string {
	permissions := make([]string, 0, len(allAPIKeyPermissionScopes()))
	for _, scope := range allAPIKeyPermissionScopes() {
		permission, ok := apiKeyScopePermission(scope)
		if !ok {
			panic("unsupported test permission " + scope)
		}
		permissions = append(permissions, string(permission))
	}
	sort.Strings(permissions)
	return permissions
}

type apiKeyPermissionCountingStore struct {
	db.Querier
	statements atomic.Int64
}

func (s *apiKeyPermissionCountingStore) reset() {
	s.statements.Store(0)
}

func (s *apiKeyPermissionCountingStore) ListAPIKeys(ctx context.Context, arg db.ListAPIKeysParams) ([]db.ListAPIKeysRow, error) {
	s.statements.Add(1)
	return s.Querier.ListAPIKeys(ctx, arg)
}

func (s *apiKeyPermissionCountingStore) IssueAPIKey(ctx context.Context, arg db.IssueAPIKeyParams) (db.APIKey, error) {
	s.statements.Add(1)
	return s.Querier.IssueAPIKey(ctx, arg)
}

func (s *apiKeyPermissionCountingStore) TouchActiveAPIKeyByTokenHash(ctx context.Context, tokenHash []byte) (db.TouchActiveAPIKeyByTokenHashRow, error) {
	s.statements.Add(1)
	return s.Querier.TouchActiveAPIKeyByTokenHash(ctx, tokenHash)
}

var _ db.Querier = (*apiKeyPermissionCountingStore)(nil)
