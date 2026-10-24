package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
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
	wantRoles := []string{"Machine workload"}
	for _, item := range first.APIKeys {
		if item.Name == "other-org" {
			t.Fatal("cross-organization API key appeared in target list")
		}
		if item.CreatedByMemberID == nil {
			t.Fatal("list omitted creator identity")
		}
		if !reflect.DeepEqual(item.RoleNames, wantRoles) {
			t.Fatalf("list permissions for %q = %+v, want %+v", item.Name, item.RoleNames, wantRoles)
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

	for _, body := range []string{`{"name":""}`, `{"name":"invalid-role","role":{"role_id":"bad"}}`, `{"name":"old-format","permissions":[{"scopes":["secrets.manage"]}]}`} {
		response := fixture.request(t, http.MethodPost, "/api/projects/api-keys/environments/production/api-keys", fixture.token, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid body status=%d: %s", response.Code, response.Body.String())
		}
	}
	fixture.store.reset()
	issuedRecorder := fixture.issueRecorder(t, "issued", fixture.role())
	if issuedRecorder.Code != http.StatusCreated || fixture.store.transactions.Load() != 1 {
		t.Fatalf("issue status/transactions = %d/%d", issuedRecorder.Code, fixture.store.transactions.Load())
	}
	var issued api.APIKeyIssued
	if err := json.Unmarshal(issuedRecorder.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(issued.RoleNames, wantRoles) || issued.RawKey == "" || issued.Status != api.APIKeyStatusActive ||
		issued.Project != "api-keys" || issued.Environment != "production" {
		t.Fatalf("issued = %+v", issued.APIKeySummary)
	}

	var creator string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT created_by_member_id::text FROM api_keys WHERE id=$1`, issued.ID).Scan(&creator); err != nil || issued.CreatedByMemberID == nil || *issued.CreatedByMemberID != creator {
		t.Fatal("creator provenance missing", err)
	}
	var attributed bool
	if err := fixture.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events a JOIN login_sessions s ON s.id=a.actor_credential_id AND s.user_id=a.actor_user_id WHERE a.action='api_key.issued' AND a.target_id=$1 AND a.actor_kind='login' AND a.project_id=$2 AND a.environment_id=$3)`, issued.ID, fixture.projectID, fixture.environmentID).Scan(&attributed); err != nil || !attributed {
		t.Fatal("issued key has no fact attributed to its authenticated login", attributed, err)
	}
	path := "/api/projects/api-keys/environments/production/api-keys/"
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
	failure := fixture.issueRecorder(t, "replacement", fixture.role())
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
	roleID        uuid.UUID
}

func newAPIKeyHTTPPostgresFixture(t *testing.T, keyCount int) apiKeyHTTPPostgresFixture {
	t.Helper()
	var store *apiKeyCountingStore
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		store = &apiKeyCountingStore{Querier: cfg.DB, TxDB: cfg.TX}
		cfg.DB = store
		cfg.TX = store
	})
	orgID, token := fixture.organizationOwner(t, "api-keys")
	otherOrgID, _ := fixture.organizationOwner(t, "api-keys-other")
	owner := func(orgID uuid.UUID) uuid.UUID {
		t.Helper()
		var userID uuid.UUID
		if err := fixture.pool.QueryRow(t.Context(), `SELECT id FROM org_members WHERE org_id = $1`, orgID).Scan(&userID); err != nil {
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
		{query: `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES ($1, $2, $3, 'production', 'Production', '#315FCE', true,1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp()), ($4, $5, $6, 'production', 'Production', '#315FCE', true,1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, args: []any{environmentID, orgID, projectID, otherEnvironmentID, otherOrgID, otherProjectID}},
	} {
		if _, err := fixture.pool.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	var ownerUser uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE id=$1`, ownerID).Scan(&ownerUser); err != nil {
		t.Fatal(err)
	}
	fixture.grant(t, orgID, ownerUser, environmentID, "keys.manage")
	roleID, _ := dbtest.CreateRole(t, fixture.pool, orgID, "agents.use", "sessions.view", "sessions.manage", "deployments.publish", "deployments.promote", "computers.operate", "secrets.manage")
	dbtest.MustExec(t, t.Context(), fixture.pool, `UPDATE roles SET name='Machine workload' WHERE id=$1`, roleID)
	keyRows := make([][]any, 0, keyCount+1)
	for index := range keyCount {
		keyRows = append(keyRows, []any{uuid.NewV7(), orgID, projectID, environmentID, ownerID, fmt.Sprintf("key-%03d", index), fmt.Sprintf("hlmr_%03d", index), []byte(fmt.Sprintf("hash-%03d", index))})
	}
	keyRows = append(keyRows, []any{uuid.NewV7(), otherOrgID, otherProjectID, otherEnvironmentID, otherOwnerID, "other-org", "hlmr_other_org", []byte("other-org-hash")})
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"api_keys"},
		[]string{"id", "org_id", "project_id", "environment_id", "created_by_member_id", "name", "key_prefix", "token_hash"}, pgx.CopyFromRows(keyRows)); err != nil {
		t.Fatal(err)
	}
	for _, row := range keyRows[:keyCount] {
		dbtest.MustExec(t, t.Context(), fixture.pool, `INSERT INTO role_grants(id,org_id,role_id,api_key_id,environment_id,agents_mode) VALUES($1,$2,$3,$4,$5,'all')`, uuid.NewV7(), orgID, roleID, row[0], environmentID)
	}
	return apiKeyHTTPPostgresFixture{httpPostgresFixture: fixture, store: store, token: token, projectID: projectID, environmentID: environmentID, roleID: roleID}
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
	return f.request(t, http.MethodGet, "/api/projects/api-keys/environments/production/api-keys?"+query, f.token, "")
}

func (f apiKeyHTTPPostgresFixture) issueRecorder(t *testing.T, name string, role *api.APIKeyRoleAssignment) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(api.IssueAPIKeyRequest{Name: name, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, http.MethodPost, "/api/projects/api-keys/environments/production/api-keys", f.token, string(body))
}

func (f apiKeyHTTPPostgresFixture) role() *api.APIKeyRoleAssignment {
	return &api.APIKeyRoleAssignment{RoleID: f.roleID.String(), ExpectedRoleRevision: 1, AgentsMode: "all", AgentIDs: []string{}}
}

type apiKeyCountingStore struct {
	db.Querier
	db.TxDB
	statements   atomic.Int64
	transactions atomic.Int64
}

func (s *apiKeyCountingStore) reset() {
	s.statements.Store(0)
	s.transactions.Store(0)
}

func (s *apiKeyCountingStore) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.HasPrefix(sql, "-- name: ListAPIKeys :many") {
		s.statements.Add(1)
	}
	return s.TxDB.Query(ctx, sql, args...)
}

func (s *apiKeyCountingStore) Begin(ctx context.Context) (pgx.Tx, error) {
	s.transactions.Add(1)
	return s.TxDB.Begin(ctx)
}

func TestAPIKeyHTTPAccessAdministratorListsWithoutLifecycleAuthority(t *testing.T) {
	f := newAPIKeyHTTPPostgresFixture(t, 1)
	ctx := t.Context()
	var orgID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.environmentID).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	user := f.user(t, "Access administrator")
	member := f.member(t, orgID, user, "member")
	dbtest.GrantMember(t, f.pool, orgID, user, nil, "access.manage")
	token := f.session(t, user, orgID)
	base := "/api/projects/api-keys/environments/production/api-keys"
	response := f.request(t, http.MethodGet, base, token, "")
	var listed api.ListAPIKeysResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listed) != nil || len(listed.APIKeys) != 1 {
		t.Fatalf("access-only list: %d %s", response.Code, response.Body.String())
	}
	keyID := listed.APIKeys[0].ID
	if bytes.Contains(response.Body.Bytes(), []byte("raw_key")) || bytes.Contains(response.Body.Bytes(), []byte("token_hash")) {
		t.Fatal("listing exposed credentials")
	}
	// Ordinary assignment uses access administration without key lifecycle authority.
	body, _ := json.Marshal(map[string]any{"role_id": f.roleID, "expected_role_revision": 1, "subject": map[string]string{"type": "api_key", "api_key_id": keyID}, "project_id": f.projectID, "environment_id": f.environmentID, "agents_mode": "all", "agent_ids": []string{}})
	if response := f.request(t, http.MethodPost, "/api/role-grants", token, string(body)); response.Code != http.StatusOK {
		t.Fatalf("access-only assignment: %d %s", response.Code, response.Body.String())
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, base, `{"name":"unauthorized"}`},
		{http.MethodPut, base + "/" + keyID, `{"name":"renamed"}`},
		{http.MethodDelete, base + "/" + keyID, ""},
		{http.MethodPost, base + "/" + keyID + "/rotate", ""},
	} {
		if response := f.request(t, tc.method, tc.path, token, tc.body); response.Code != http.StatusForbidden {
			t.Fatalf("access-only %s %s: %d %s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM permission_group_members WHERE member_id=$1; DELETE FROM role_grants WHERE member_id=$1`, pgx.QueryExecModeSimpleProtocol, member)
	if response := f.request(t, http.MethodGet, base, token, ""); response.Code != http.StatusForbidden {
		t.Fatalf("unprivileged list: %d %s", response.Code, response.Body.String())
	}
	if response := f.request(t, http.MethodGet, base, "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d %s", response.Code, response.Body.String())
	}
}
