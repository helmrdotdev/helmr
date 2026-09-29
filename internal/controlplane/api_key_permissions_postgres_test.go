package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// TestAPIKeyHTTPPostgresContract covers the HTTP contract of the API key
// handlers; the durable behavior behind them is tested in internal/identity.
func TestAPIKeyHTTPPostgresContract(t *testing.T) {
	fixture := newAPIKeyHTTPPostgresFixture(t, 201)

	fixture.store.reset()
	first := fixture.list(t, "active", "")
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
		if !reflect.DeepEqual(item.Permissions, wantGrants) {
			t.Fatalf("list permissions for %q = %+v, want %+v", item.Name, item.Permissions, wantGrants)
		}
	}
	fixture.store.reset()
	second := fixture.list(t, "active", first.NextCursor)
	if len(second.APIKeys) != 1 || second.NextCursor != "" || fixture.store.statements.Load() != 1 {
		t.Fatalf("second list items/cursor/statements = %d/%t/%d, want 1/false/1", len(second.APIKeys), second.NextCursor != "", fixture.store.statements.Load())
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
	for _, query := range []string{"filter=active&cursor=" + crossScope, "filter=active&cursor=not-a-cursor", "filter=revoked&cursor=" + first.NextCursor, "filter=pending"} {
		fixture.store.reset()
		if response := fixture.listRecorder(t, query); response.Code != http.StatusBadRequest || fixture.store.statements.Load() != 0 {
			t.Fatalf("%s status/statements = %d/%d, want 400/0", query, response.Code, fixture.store.statements.Load())
		}
	}

	for _, test := range []struct {
		name   string
		grants []api.APIKeyPermissionGrant
	}{
		{name: "empty"},
		{name: "empty grant", grants: []api.APIKeyPermissionGrant{{}}},
		{name: "unsupported", grants: []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{"unsupported"}}}},
		{name: "", grants: []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{api.APIKeyScopeRunsRead}}}},
	} {
		fixture.store.reset()
		response := fixture.issueRecorder(t, test.name, test.grants)
		if response.Code != http.StatusBadRequest || fixture.store.statements.Load() != 0 {
			t.Fatalf("%q status/statements = %d/%d, want 400/0", test.name, response.Code, fixture.store.statements.Load())
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
	if !reflect.DeepEqual(issued.Permissions, wantGrants) || issued.RawKey == "" || issued.Status != api.APIKeyStatusActive ||
		issued.ProjectID != fixture.projectID.String() || issued.EnvironmentID != fixture.environmentID.String() {
		t.Fatalf("issued = %+v", issued.APIKeySummary)
	}

	path := fmt.Sprintf("/api/projects/%s/environments/%s/api-keys/", fixture.projectID, fixture.environmentID)
	if response := fixture.request(t, http.MethodDelete, path+issued.ID, fixture.token, ""); response.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d: %s", response.Code, response.Body.String())
	}
	for _, id := range []string{issued.ID, uuid.NewV7().String(), "not-an-id"} {
		if response := fixture.request(t, http.MethodDelete, path+id, fixture.token, ""); response.Code != http.StatusNotFound {
			t.Fatalf("revoke %s status = %d: %s", id, response.Code, response.Body.String())
		}
	}
	revoked := fixture.list(t, "revoked", "")
	if len(revoked.APIKeys) != 1 || revoked.APIKeys[0].ID != issued.ID || revoked.APIKeys[0].Status != api.APIKeyStatusRevoked {
		t.Fatalf("revoked list = %+v", revoked.APIKeys)
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
	failure := fixture.issueRecorder(t, "replacement", []api.APIKeyPermissionGrant{{Scopes: allAPIKeyPermissionScopes()}})
	if failure.Code != http.StatusInternalServerError || bytes.Contains(failure.Body.Bytes(), []byte(`"raw_key"`)) {
		t.Fatalf("replacement failure status/body = %d/%s", failure.Code, failure.Body.String())
	}
}

type apiKeyHTTPPostgresFixture struct {
	httpPostgresFixture
	store         *apiKeyCountingStore
	token         string
	projectID     uuid.UUID
	environmentID uuid.UUID
}

func newAPIKeyHTTPPostgresFixture(t *testing.T, keyCount int) apiKeyHTTPPostgresFixture {
	t.Helper()
	var store *apiKeyCountingStore
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		store = &apiKeyCountingStore{Querier: cfg.DB}
		cfg.DB = store
	})
	orgID, token := fixture.organizationOwner(t, "api-keys")
	otherOrgID, _ := fixture.organizationOwner(t, "api-keys-other")
	owner := func(orgID uuid.UUID) uuid.UUID {
		t.Helper()
		var userID uuid.UUID
		if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id = $1`, orgID).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		return userID
	}
	ownerID, otherOwnerID := owner(orgID), owner(otherOrgID)
	projectID, environmentID := uuid.NewV7(), uuid.NewV7()
	otherProjectID, otherEnvironmentID := uuid.NewV7(), uuid.NewV7()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{query: `INSERT INTO regions (id, display_name) VALUES ('api-keys', 'API keys')`},
		{query: `INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default) VALUES ($1, $2, 'api-keys', 'api-keys', 'API keys', true), ($3, $4, 'api-keys', 'other', 'Other', true)`, args: []any{projectID, orgID, otherProjectID, otherOrgID}},
		{query: `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default) VALUES ($1, $2, $3, 'production', 'Production', '#315FCE', true), ($4, $5, $6, 'production', 'Production', '#315FCE', true)`, args: []any{environmentID, orgID, projectID, otherEnvironmentID, otherOrgID, otherProjectID}},
	} {
		if _, err := fixture.pool.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	permissions := allAPIKeyInternalPermissions()
	keyRows := make([][]any, 0, keyCount+1)
	for index := range keyCount {
		keyRows = append(keyRows, []any{uuid.NewV7(), orgID, projectID, environmentID, ownerID, "owner", permissions, fmt.Sprintf("key-%03d", index), fmt.Sprintf("hlmr_%03d", index), []byte(fmt.Sprintf("hash-%03d", index))})
	}
	keyRows = append(keyRows, []any{uuid.NewV7(), otherOrgID, otherProjectID, otherEnvironmentID, otherOwnerID, "owner", permissions, "other-org", "hlmr_other_org", []byte("other-org-hash")})
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"api_keys"},
		[]string{"id", "org_id", "project_id", "environment_id", "created_by_user_id", "role", "permissions", "name", "key_prefix", "token_hash"}, pgx.CopyFromRows(keyRows)); err != nil {
		t.Fatal(err)
	}
	return apiKeyHTTPPostgresFixture{httpPostgresFixture: fixture, store: store, token: token, projectID: projectID, environmentID: environmentID}
}

func (f apiKeyHTTPPostgresFixture) list(t *testing.T, filter string, cursor string) api.ListAPIKeysResponse {
	t.Helper()
	query := "filter=" + filter
	if cursor != "" {
		query += "&cursor=" + cursor
	}
	recorder := f.listRecorder(t, query)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response api.ListAPIKeysResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func (f apiKeyHTTPPostgresFixture) listRecorder(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return f.request(t, http.MethodGet, fmt.Sprintf("/api/projects/%s/environments/%s/api-keys?%s", f.projectID, f.environmentID, query), f.token, "")
}

func (f apiKeyHTTPPostgresFixture) issueRecorder(t *testing.T, name string, grants []api.APIKeyPermissionGrant) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(api.IssueAPIKeyRequest{Name: name, Permissions: grants})
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, http.MethodPost, fmt.Sprintf("/api/projects/%s/environments/%s/api-keys", f.projectID, f.environmentID), f.token, string(body))
}

func allAPIKeyPermissionScopes() []api.APIKeyScope {
	return []api.APIKeyScope{
		api.APIKeyScopeRunsCreate, api.APIKeyScopeRunsRead, api.APIKeyScopeRunsManage,
		api.APIKeyScopeSessionsRead, api.APIKeyScopeActorsStart, api.APIKeyScopeSessionsSend,
		api.APIKeyScopeSessionsClose, api.APIKeyScopeSessionsInterrupt, api.APIKeyScopeSessionsResume, api.APIKeyScopeTokensCreate, api.APIKeyScopeTokensRead,
		api.APIKeyScopeTokensComplete, api.APIKeyScopeTokensCancel, api.APIKeyScopeComputersCreate,
		api.APIKeyScopeComputersRead, api.APIKeyScopeComputersDelete,
		api.APIKeyScopeComputerCommandCreate, api.APIKeyScopeSecretsWrite, api.APIKeyScopeTasksDeploy,
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

type apiKeyCountingStore struct {
	db.Querier
	statements atomic.Int64
}

func (s *apiKeyCountingStore) reset() {
	s.statements.Store(0)
}

func (s *apiKeyCountingStore) ListAPIKeys(ctx context.Context, arg db.ListAPIKeysParams) ([]db.ListAPIKeysRow, error) {
	s.statements.Add(1)
	return s.Querier.ListAPIKeys(ctx, arg)
}

func (s *apiKeyCountingStore) IssueAPIKey(ctx context.Context, arg db.IssueAPIKeyParams) (db.APIKey, error) {
	s.statements.Add(1)
	return s.Querier.IssueAPIKey(ctx, arg)
}
