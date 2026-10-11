package identity

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type apiKeyFixture struct {
	identityFixture
	owner auth.Principal
	scope auth.Scope
}

func newAPIKeyFixture(t *testing.T) apiKeyFixture {
	t.Helper()
	fixture := newIdentityFixture(t)
	orgID := fixture.organization(t, "api-keys")
	userID := fixture.user(t, "Owner", "")
	fixture.member(t, orgID, userID, db.OrgMemberRoleOwner)
	projectID, environmentID := uuid.NewV7(), uuid.NewV7()
	fixture.exec(t, `INSERT INTO regions (id, display_name) VALUES ('api-keys', 'API keys')`)
	fixture.exec(t, `INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default) VALUES ($1, $2, 'api-keys', 'api-keys', 'API keys', true)`, projectID, orgID)
	fixture.exec(t, `INSERT INTO environments (history_retention_mode,id, org_id, project_id, slug, name, color_hex, is_default) VALUES ('until_environment_deletion',$1, $2, $3, 'production', 'Production', '#315FCE', true)`, environmentID, orgID, projectID)
	return apiKeyFixture{
		identityFixture: fixture,
		owner:           auth.Principal{OrgID: orgID, UserID: userID, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner},
		scope:           auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: environmentID.String()},
	}
}

func (f apiKeyFixture) issue(t *testing.T, name string, permissions ...auth.Permission) IssuedAPIKey {
	t.Helper()
	issued, err := IssueAPIKey(t.Context(), f.queries, f.owner, f.scope, APIKeyInput{Name: name, Permissions: permissions})
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func TestAPIKeyPostgresAuthenticatesActiveKeys(t *testing.T) {
	fixture := newAPIKeyFixture(t)
	ctx := t.Context()
	store := &apiKeyCountingQuerier{Querier: fixture.queries}
	authenticator := NewAPIKeyAuthenticator(store)
	issued := fixture.issue(t, "  deploy  ", auth.PermissionSessionsRead, auth.PermissionDeploymentsWrite)
	if issued.Record.Name != "deploy" || issued.Raw == "" || !slices.Equal(issued.Record.Permissions, []string{"sessions.read", "deployments.write"}) {
		t.Fatalf("issued = %+v", issued.Record)
	}

	principal, err := authenticator.Authenticate(ctx, " "+issued.Raw+" ")
	if err != nil || store.reset() != 1 {
		t.Fatalf("authenticate error = %v, want one statement", err)
	}
	want := auth.Principal{
		OrgID: fixture.owner.OrgID, APIKeyID: pgvalue.MustUUIDValue(issued.Record.ID),
		ProjectID: fixture.scope.ProjectID, EnvironmentID: fixture.scope.EnvironmentID,
		Kind: auth.PrincipalKindAPIKey, Role: auth.RoleOwner,
		Permissions: []auth.Permission{auth.PermissionSessionsRead, auth.PermissionDeploymentsWrite},
	}
	if !reflect.DeepEqual(principal, want) {
		t.Fatalf("principal = %+v, want %+v", principal, want)
	}
	var touchedAt *time.Time
	if err := fixture.pool.QueryRow(ctx, `SELECT last_used_at FROM api_keys WHERE id = $1`, issued.Record.ID).Scan(&touchedAt); err != nil || touchedAt == nil {
		t.Fatalf("last_used_at = %v, err = %v", touchedAt, err)
	}

	fixture.exec(t, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, issued.Record.ID)
	if _, err := authenticator.Authenticate(ctx, issued.Raw); !errors.Is(err, auth.ErrUnauthenticated) || store.reset() != 1 {
		t.Fatalf("revoked key error = %v, want one statement", err)
	}
	var revokedTouchedAt *time.Time
	if err := fixture.pool.QueryRow(ctx, `SELECT last_used_at FROM api_keys WHERE id = $1`, issued.Record.ID).Scan(&revokedTouchedAt); err != nil || revokedTouchedAt == nil || !revokedTouchedAt.Equal(*touchedAt) {
		t.Fatalf("revoked key last_used_at = %v, want %v, err = %v", revokedTouchedAt, touchedAt, err)
	}
	expired := fixture.issue(t, "expired", auth.PermissionSessionsRead)
	fixture.expire(t, "api_keys", expired.Record.ID)
	if _, err := authenticator.Authenticate(ctx, expired.Raw); !errors.Is(err, auth.ErrUnauthenticated) || store.reset() != 1 {
		t.Fatalf("expired key error = %v, want one statement", err)
	}
	var untouched bool
	if err := fixture.pool.QueryRow(ctx, `SELECT last_used_at IS NULL FROM api_keys WHERE id = $1`, expired.Record.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("expired key touched = %t, err = %v", !untouched, err)
	}
	for _, raw := range []string{"", " ", "hlmr_unknown"} {
		if _, err := authenticator.Authenticate(ctx, raw); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("key %q error = %v", raw, err)
		}
	}
}

func TestAPIKeyPostgresReplacementIsAtomic(t *testing.T) {
	fixture := newAPIKeyFixture(t)
	ctx := t.Context()
	original := fixture.issue(t, "replacement", auth.PermissionSessionsRead)
	replacement := fixture.issue(t, "replacement", auth.PermissionSessionsRead, auth.PermissionAgentsStart)
	var originalRevoked bool
	if err := fixture.pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1`, original.Record.ID).Scan(&originalRevoked); err != nil || !originalRevoked {
		t.Fatalf("original revoked = %t, err = %v", originalRevoked, err)
	}

	fixture.exec(t, `
		CREATE FUNCTION reject_replacement_api_key() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.name = 'replacement' THEN
				RAISE EXCEPTION 'injected replacement failure';
			END IF;
			RETURN NEW;
		END
		$$`)
	fixture.exec(t, `CREATE TRIGGER reject_replacement_api_key BEFORE INSERT ON api_keys FOR EACH ROW EXECUTE FUNCTION reject_replacement_api_key()`)
	store := &apiKeyCountingQuerier{Querier: fixture.queries}
	if _, err := IssueAPIKey(ctx, store, fixture.owner, fixture.scope, APIKeyInput{Name: "replacement", Permissions: []auth.Permission{auth.PermissionSessionsRead}}); err == nil || store.reset() != 1 {
		t.Fatalf("failed replacement error = %v, want an error from one statement", err)
	}
	var activeID uuid.UUID
	var activePermissions []string
	if err := fixture.pool.QueryRow(ctx, `SELECT id, permissions FROM api_keys WHERE name = 'replacement' AND revoked_at IS NULL`).Scan(&activeID, &activePermissions); err != nil ||
		activeID != pgvalue.MustUUIDValue(replacement.Record.ID) || !slices.Equal(activePermissions, replacement.Record.Permissions) {
		t.Fatalf("active replacement = %s %v, err = %v", activeID, activePermissions, err)
	}
}

func TestAPIKeyPostgresListsAndRevokesWithinEnvironment(t *testing.T) {
	fixture := newAPIKeyFixture(t)
	ctx := t.Context()
	rows := make([][]any, 0, 5)
	for index := range 5 {
		rows = append(rows, []any{uuid.NewV7(), fixture.owner.OrgID, uuid.MustParse(fixture.scope.ProjectID), uuid.MustParse(fixture.scope.EnvironmentID), fixture.owner.UserID, "owner", []string{"sessions.read"}, fmt.Sprintf("key-%d", index), fmt.Sprintf("hlmr_%d", index), []byte(fmt.Sprintf("hash-%d", index))})
	}
	if _, err := fixture.pool.CopyFrom(ctx, pgx.Identifier{"api_keys"},
		[]string{"id", "org_id", "project_id", "environment_id", "created_by_user_id", "role", "permissions", "name", "key_prefix", "token_hash"}, pgx.CopyFromRows(rows)); err != nil {
		t.Fatal(err)
	}
	otherOrg := fixture.organization(t, "other")
	other := auth.Principal{OrgID: otherOrg, UserID: fixture.user(t, "Other", ""), Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}
	fixture.member(t, otherOrg, other.UserID, db.OrgMemberRoleOwner)
	otherProjectID, otherEnvironmentID := uuid.NewV7(), uuid.NewV7()
	fixture.exec(t, `INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default) VALUES ($1, $2, 'api-keys', 'other', 'Other', true)`, otherProjectID, otherOrg)
	fixture.exec(t, `INSERT INTO environments (history_retention_mode,id, org_id, project_id, slug, name, color_hex, is_default) VALUES ('until_environment_deletion',$1, $2, $3, 'production', 'Production', '#315FCE', true)`, otherEnvironmentID, otherOrg, otherProjectID)
	otherScope := auth.Scope{OrgID: otherOrg, ProjectID: otherProjectID.String(), EnvironmentID: otherEnvironmentID.String()}
	if _, err := IssueAPIKey(ctx, fixture.queries, other, otherScope, APIKeyInput{Name: "other-org", Permissions: []auth.Permission{auth.PermissionSessionsRead}}); err != nil {
		t.Fatal(err)
	}
	if listed, _, err := ListAPIKeys(ctx, fixture.queries, other, fixture.scope, APIKeyFilterAll, 10, nil); err != nil || len(listed) != 0 {
		t.Fatalf("cross-organization list = %d, err = %v", len(listed), err)
	}

	first, more, err := ListAPIKeys(ctx, fixture.queries, fixture.owner, fixture.scope, APIKeyFilterActive, 3, nil)
	if err != nil || len(first) != 3 || !more {
		t.Fatalf("first page = %d/%t, err = %v", len(first), more, err)
	}
	last := first[len(first)-1]
	after := &APIKeyPosition{CreatedAt: last.CreatedAt.Time, ID: pgvalue.MustUUIDValue(last.ID)}
	second, more, err := ListAPIKeys(ctx, fixture.queries, fixture.owner, fixture.scope, APIKeyFilterActive, 3, after)
	if err != nil || len(second) != 2 || more {
		t.Fatalf("second page = %d/%t, err = %v", len(second), more, err)
	}

	revokedID := pgvalue.MustUUIDValue(second[0].ID)
	if err := RevokeAPIKey(ctx, fixture.queries, other, fixture.scope, revokedID); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("cross-organization revoke error = %v", err)
	}
	if err := RevokeAPIKey(ctx, fixture.queries, fixture.owner, fixture.scope, revokedID); err != nil {
		t.Fatal(err)
	}
	if err := RevokeAPIKey(ctx, fixture.queries, fixture.owner, fixture.scope, revokedID); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("repeated revoke error = %v", err)
	}
	revoked, _, err := ListAPIKeys(ctx, fixture.queries, fixture.owner, fixture.scope, APIKeyFilterRevoked, 10, nil)
	if err != nil || len(revoked) != 1 || revoked[0].ID != second[0].ID {
		t.Fatalf("revoked list = %+v, err = %v", revoked, err)
	}
	active, _, err := ListAPIKeys(ctx, fixture.queries, fixture.owner, fixture.scope, APIKeyFilterActive, 10, nil)
	if err != nil || len(active) != 4 {
		t.Fatalf("active list = %d, err = %v", len(active), err)
	}

	// Listing reports each key's stored permissions, including a key holding
	// fewer than another.
	full := fixture.issue(t, "full", auth.PermissionSessionsRead, auth.PermissionDeploymentsWrite)
	expired := fixture.issue(t, "expired", auth.PermissionSessionsRead)
	fixture.expire(t, "api_keys", expired.Record.ID)
	permissions := map[string][]string{}
	all, _, err := ListAPIKeys(ctx, fixture.queries, fixture.owner, fixture.scope, APIKeyFilterAll, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range all {
		permissions[row.Name] = row.Permissions
	}
	if len(all) != 7 || permissions["other-org"] != nil || permissions[second[0].Name] == nil || permissions["expired"] == nil ||
		!slices.Equal(permissions["full"], full.Record.Permissions) || !slices.Equal(permissions["key-0"], []string{"sessions.read"}) {
		t.Fatalf("all list = %v", permissions)
	}
	expiredList, _, err := ListAPIKeys(ctx, fixture.queries, fixture.owner, fixture.scope, APIKeyFilterExpired, 10, nil)
	if err != nil || len(expiredList) != 1 || expiredList[0].ID != expired.Record.ID {
		t.Fatalf("expired list = %+v, err = %v", expiredList, err)
	}
}

func TestAPIKeyPostgresRequiresManagementAndValidInput(t *testing.T) {
	fixture := newAPIKeyFixture(t)
	ctx := t.Context()
	for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleViewer} {
		principal := fixture.owner
		principal.Role = role
		if _, err := IssueAPIKey(ctx, fixture.queries, principal, fixture.scope, APIKeyInput{Name: "blocked", Permissions: []auth.Permission{auth.PermissionSessionsRead}}); !errors.Is(err, ErrAPIKeyManagementRequired) {
			t.Fatalf("%s issue error = %v", role, err)
		}
		if _, _, err := ListAPIKeys(ctx, fixture.queries, principal, fixture.scope, APIKeyFilterAll, 10, nil); !errors.Is(err, ErrAPIKeyManagementRequired) {
			t.Fatalf("%s list error = %v", role, err)
		}
		if err := RevokeAPIKey(ctx, fixture.queries, principal, fixture.scope, uuid.NewV7()); !errors.Is(err, ErrAPIKeyManagementRequired) {
			t.Fatalf("%s revoke error = %v", role, err)
		}
	}
	days, invalidDays := 90, 31
	for _, input := range []APIKeyInput{
		{Name: "", Permissions: []auth.Permission{auth.PermissionSessionsRead}},
		{Name: "bad\nname", Permissions: []auth.Permission{auth.PermissionSessionsRead}},
		{Name: string(make([]byte, 65)), Permissions: []auth.Permission{auth.PermissionSessionsRead}},
		{Name: "no permissions"},
		{Name: "member management", Permissions: []auth.Permission{auth.PermissionMembersManage}},
		{Name: "expiry", Permissions: []auth.Permission{auth.PermissionSessionsRead}, ExpiresInDays: &invalidDays},
	} {
		var inputError InputError
		if _, err := IssueAPIKey(ctx, fixture.queries, fixture.owner, fixture.scope, input); !errors.As(err, &inputError) {
			t.Fatalf("input %+v error = %v", input, err)
		}
	}
	expiring, err := IssueAPIKey(ctx, fixture.queries, fixture.owner, fixture.scope, APIKeyInput{Name: "expiring", Permissions: []auth.Permission{auth.PermissionSessionsRead}, ExpiresInDays: &days})
	if err != nil || !expiring.Record.ExpiresAt.Valid || time.Until(expiring.Record.ExpiresAt.Time) < 89*24*time.Hour {
		t.Fatalf("expiring key = %+v, err = %v", expiring.Record.ExpiresAt, err)
	}
	var count int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("issued keys = %d, err = %v", count, err)
	}
}

func TestParseAPIKeyFilter(t *testing.T) {
	for value, want := range map[string]APIKeyFilter{"": APIKeyFilterActive, "active": APIKeyFilterActive, "expired": APIKeyFilterExpired, "revoked": APIKeyFilterRevoked, "all": APIKeyFilterAll} {
		if got, err := ParseAPIKeyFilter(value); err != nil || got != want {
			t.Fatalf("ParseAPIKeyFilter(%q) = %q, %v", value, got, err)
		}
	}
	var inputError InputError
	if _, err := ParseAPIKeyFilter("pending"); !errors.As(err, &inputError) {
		t.Fatalf("unknown filter error = %v", err)
	}
}

func TestGrantedPermissionsKeepsGrantablePermissions(t *testing.T) {
	if got := grantedPermissions([]string{" sessions.read ", "members.manage", "unknown"}); !reflect.DeepEqual(got, []auth.Permission{auth.PermissionSessionsRead}) {
		t.Fatalf("permissions = %v", got)
	}
	if got := grantedPermissions([]string{"members.manage", "unknown"}); got != nil {
		t.Fatalf("permissions = %v, want nil", got)
	}
}

type apiKeyCountingQuerier struct {
	db.Querier
	statements atomic.Int64
}

// reset returns the statements counted since the last reset.
func (q *apiKeyCountingQuerier) reset() int64 {
	return q.statements.Swap(0)
}

func (q *apiKeyCountingQuerier) TouchActiveAPIKeyByTokenHash(ctx context.Context, tokenHash []byte) (db.TouchActiveAPIKeyByTokenHashRow, error) {
	q.statements.Add(1)
	return q.Querier.TouchActiveAPIKeyByTokenHash(ctx, tokenHash)
}

func (q *apiKeyCountingQuerier) IssueAPIKey(ctx context.Context, arg db.IssueAPIKeyParams) (db.APIKey, error) {
	q.statements.Add(1)
	return q.Querier.IssueAPIKey(ctx, arg)
}
