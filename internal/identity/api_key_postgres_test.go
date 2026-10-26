package identity

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
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
	fixture.member(t, orgID, userID, "owner")
	projectID, environmentID := uuid.NewV7(), uuid.NewV7()
	fixture.exec(t, `INSERT INTO regions (id, display_name) VALUES ('api-keys', 'API keys')`)
	fixture.exec(t, `INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default) VALUES ($1, $2, 'api-keys', 'api-keys', 'API keys', true)`, projectID, orgID)
	fixture.exec(t, `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES ($1, $2, $3, 'production', 'Production', '#315FCE', true,1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, environmentID, orgID, projectID)
	dbtest.GrantMember(t, fixture.pool, orgID, userID, nil, "members.read", "members.remove", "invitations.read", "invitations.create", "invitations.revoke", "groups.read", "groups.create", "groups.update", "groups.delete", "groups.add_member", "groups.remove_member", "roles.read", "roles.create", "roles.update", "roles.delete", "assignments.read", "assignments.create", "assignments.update", "assignments.delete")
	dbtest.GrantMember(t, fixture.pool, orgID, userID, &environmentID, "api_keys.read", "api_keys.create", "api_keys.update", "api_keys.rotate", "api_keys.revoke")
	principal, err := access.Principal(t.Context(), fixture.pool, "user", userID, environmentID)
	if err != nil {
		t.Fatal(err)
	}
	principal.SessionID = uuid.NewV7()
	return apiKeyFixture{
		identityFixture: fixture,
		owner:           principal,
		scope:           auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: environmentID.String()},
	}
}

func (f apiKeyFixture) issue(t *testing.T, name string, permissions ...auth.Permission) IssuedAPIKey {
	t.Helper()
	issued, err := IssueAPIKey(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, APIKeyInput{Name: name, Role: f.role(t, permissions...)})
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func (f apiKeyFixture) role(t *testing.T, permissions ...auth.Permission) *APIKeyRole {
	t.Helper()
	values := make([]string, 0, len(permissions))
	for _, p := range permissions {
		values = append(values, string(p))
	}
	id, mode := dbtest.CreateRole(t, f.pool, f.owner.OrgID, values...)
	return &APIKeyRole{RoleID: id, ExpectedRoleRevision: 1, AgentsMode: mode}
}

func TestAPIKeyPostgresAuthenticatesActiveKeys(t *testing.T) {
	fixture := newAPIKeyFixture(t)
	ctx := t.Context()
	store := &apiKeyCountingQuerier{DBTX: fixture.pool}
	authenticator := NewAPIKeyAuthenticator(store)
	issued := fixture.issue(t, "  deploy  ", auth.PermissionSessionsRead, auth.PermissionDeploymentsPublish)
	if issued.Record.Name != "deploy" || issued.Raw == "" {
		t.Fatalf("issued = %+v", issued.Record)
	}

	principal, err := authenticator.Authenticate(ctx, " "+issued.Raw+" ")
	if err != nil || store.reset() != 2 {
		t.Fatalf("authenticate error = %v, want token and current grants queries", err)
	}
	want := auth.Principal{CredentialVersion: 1,
		OrgID: fixture.owner.OrgID, APIKeyID: pgvalue.MustUUIDValue(issued.Record.ID),
		ProjectID: fixture.scope.ProjectID, EnvironmentID: fixture.scope.EnvironmentID,
		Kind:   auth.PrincipalKindAPIKey,
		Grants: []auth.PermissionGrant{{ProjectID: fixture.scope.ProjectID, EnvironmentID: fixture.scope.EnvironmentID, Permission: auth.PermissionDeploymentsPublish}, {ProjectID: fixture.scope.ProjectID, EnvironmentID: fixture.scope.EnvironmentID, Permission: auth.PermissionSessionsRead}},
	}
	for i := range want.Grants {
		g := principal.Grants[i]
		if g.AssignmentID == "" || g.AssignmentRevision != 1 || g.RoleID == "" || g.RoleRevision != 1 || g.Subject != "api_key" {
			t.Fatalf("missing credential grant witness: %+v", g)
		}
		want.Grants[i].AssignmentID = g.AssignmentID
		want.Grants[i].AssignmentRevision = g.AssignmentRevision
		want.Grants[i].RoleID = g.RoleID
		want.Grants[i].RoleRevision = g.RoleRevision
		want.Grants[i].Subject = g.Subject
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

func TestAPIKeyDuplicateNameAndRotation(t *testing.T) {
	f := newAPIKeyFixture(t)
	ctx := t.Context()
	original := f.issue(t, "automation", auth.PermissionAgentsUse)
	if _, err := IssueAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, APIKeyInput{Name: "automation"}); err == nil {
		t.Fatal("reserved name accepted")
	}
	rotated, err := RotateAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, pgvalue.MustUUIDValue(original.Record.ID))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Record.ID != original.Record.ID || rotated.Raw == original.Raw {
		t.Fatal("rotation changed machine identity or permissions")
	}
	authenticator := NewAPIKeyAuthenticator(f.pool)
	if _, err := authenticator.Authenticate(ctx, original.Raw); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("old token survived", err)
	}
	if _, err := authenticator.Authenticate(ctx, rotated.Raw); err != nil {
		t.Fatal(err)
	}
	f.expire(t, "api_keys", original.Record.ID)
	if _, err := RotateAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, pgvalue.MustUUIDValue(original.Record.ID)); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatal("expired key rotated", err)
	}
	if _, err := IssueAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, APIKeyInput{Name: "automation"}); err == nil {
		t.Fatal("expired key did not reserve name")
	}
	if err := RevokeAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, pgvalue.MustUUIDValue(original.Record.ID)); err != nil {
		t.Fatal(err)
	}
	replacement := f.issue(t, "automation", auth.PermissionAgentsUse)
	if replacement.Record.ID == original.Record.ID {
		t.Fatal("replacement reused identity")
	}
}

func TestAPIKeyPostgresListsAndRevokesWithinEnvironment(t *testing.T) {
	fixture := newAPIKeyFixture(t)
	ctx := t.Context()
	rows := make([][]any, 0, 5)
	for index := range 5 {
		rows = append(rows, []any{uuid.NewV7(), fixture.owner.OrgID, uuid.MustParse(fixture.scope.ProjectID), uuid.MustParse(fixture.scope.EnvironmentID), fixture.owner.MemberID, fmt.Sprintf("key-%d", index), fmt.Sprintf("hlmr_%d", index), []byte(fmt.Sprintf("hash-%d", index))})
	}
	if _, err := fixture.pool.CopyFrom(ctx, pgx.Identifier{"api_keys"},
		[]string{"id", "org_id", "project_id", "environment_id", "created_by_member_id", "name", "key_prefix", "token_hash"}, pgx.CopyFromRows(rows)); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		dbtest.SetAPIKeyRole(t, fixture.pool, row[0].(uuid.UUID), "sessions.read")
	}
	otherOrg := fixture.organization(t, "other")
	other := auth.Principal{OrgID: otherOrg, UserID: fixture.user(t, "Other", ""), Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7()}
	fixture.member(t, otherOrg, other.UserID, "owner")
	otherProjectID, otherEnvironmentID := uuid.NewV7(), uuid.NewV7()
	fixture.exec(t, `INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default) VALUES ($1, $2, 'api-keys', 'other', 'Other', true)`, otherProjectID, otherOrg)
	fixture.exec(t, `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES ($1, $2, $3, 'production', 'Production', '#315FCE', true,1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, otherEnvironmentID, otherOrg, otherProjectID)
	other.MemberID = dbtest.GrantMember(t, fixture.pool, otherOrg, other.UserID, &otherEnvironmentID, "api_keys.read", "api_keys.create", "api_keys.update", "api_keys.rotate", "api_keys.revoke")
	other.Grants = []auth.PermissionGrant{{ProjectID: otherProjectID.String(), EnvironmentID: otherEnvironmentID.String(), Permission: auth.PermissionAPIKeysCreate}}
	otherScope := auth.Scope{OrgID: otherOrg, ProjectID: otherProjectID.String(), EnvironmentID: otherEnvironmentID.String()}
	if _, err := IssueAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, other, otherScope, APIKeyInput{Name: "other-org"}); err != nil {
		t.Fatal(err)
	}
	if listed, _, _, err := ListAPIKeys(ctx, fixture.pool, other, fixture.scope, APIKeyFilterAll, 10, nil); !errors.Is(err, ErrAPIKeyManagementRequired) || len(listed) != 0 {
		t.Fatalf("cross-organization list = %d, err = %v", len(listed), err)
	}

	first, more, _, err := ListAPIKeys(ctx, fixture.pool, fixture.owner, fixture.scope, APIKeyFilterActive, 3, nil)
	if err != nil || len(first) != 3 || !more {
		t.Fatalf("first page = %d/%t, err = %v", len(first), more, err)
	}
	last := first[len(first)-1]
	after := &APIKeyPosition{CreatedAt: last.CreatedAt.Time, ID: pgvalue.MustUUIDValue(last.ID)}
	second, more, _, err := ListAPIKeys(ctx, fixture.pool, fixture.owner, fixture.scope, APIKeyFilterActive, 3, after)
	if err != nil || len(second) != 2 || more {
		t.Fatalf("second page = %d/%t, err = %v", len(second), more, err)
	}

	revokedID := pgvalue.MustUUIDValue(second[0].ID)
	if err := RevokeAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, other, fixture.scope, revokedID); !errors.Is(err, ErrAPIKeyManagementRequired) {
		t.Fatalf("cross-organization revoke error = %v", err)
	}
	if err := RevokeAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, fixture.owner, fixture.scope, revokedID); err != nil {
		t.Fatal(err)
	}
	if err := RevokeAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, fixture.owner, fixture.scope, revokedID); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("repeated revoke error = %v", err)
	}
	revoked, _, _, err := ListAPIKeys(ctx, fixture.pool, fixture.owner, fixture.scope, APIKeyFilterRevoked, 10, nil)
	if err != nil || len(revoked) != 1 || revoked[0].ID != second[0].ID {
		t.Fatalf("revoked list = %+v, err = %v", revoked, err)
	}
	active, _, _, err := ListAPIKeys(ctx, fixture.pool, fixture.owner, fixture.scope, APIKeyFilterActive, 10, nil)
	if err != nil || len(active) != 4 {
		t.Fatalf("active list = %d, err = %v", len(active), err)
	}

	// Listing reports each key's stored permissions, including a key holding
	// fewer than another.
	full := fixture.issue(t, "full", auth.PermissionSessionsRead, auth.PermissionDeploymentsPublish)
	expired := fixture.issue(t, "expired", auth.PermissionSessionsRead)
	fixture.expire(t, "api_keys", expired.Record.ID)
	permissions := map[string][]string{}
	all, _, _, err := ListAPIKeys(ctx, fixture.pool, fixture.owner, fixture.scope, APIKeyFilterAll, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range all {
		permissions[row.Name] = row.RoleNames
	}
	if len(all) != 7 || permissions["other-org"] != nil || permissions[second[0].Name] == nil || permissions["expired"] == nil ||
		!slices.Equal(permissions["full"], full.RoleNames) || len(permissions["key-0"]) != 1 {
		t.Fatalf("all list = %v", permissions)
	}
	expiredList, _, _, err := ListAPIKeys(ctx, fixture.pool, fixture.owner, fixture.scope, APIKeyFilterExpired, 10, nil)
	if err != nil || len(expiredList) != 1 || expiredList[0].ID != expired.Record.ID {
		t.Fatalf("expired list = %+v, err = %v", expiredList, err)
	}
}

func TestAPIKeyPostgresRequiresManagementAndValidInput(t *testing.T) {
	fixture := newAPIKeyFixture(t)
	ctx := t.Context()
	for _, role := range []string{"member", "owner"} {
		principal := fixture.owner
		principal.UserID = fixture.user(t, "No grants "+role, "")
		principal.MemberID = fixture.member(t, principal.OrgID, principal.UserID, role)
		principal.IsOwner = role == "owner"
		principal.Grants = nil
		if _, err := IssueAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, fixture.scope, APIKeyInput{Name: "blocked"}); !errors.Is(err, ErrAPIKeyManagementRequired) {
			t.Fatalf("%s issue error = %v", role, err)
		}
		if _, _, _, err := ListAPIKeys(ctx, fixture.pool, principal, fixture.scope, APIKeyFilterAll, 10, nil); !errors.Is(err, ErrAPIKeyManagementRequired) {
			t.Fatalf("%s list error = %v", role, err)
		}
		if err := RevokeAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, fixture.scope, uuid.NewV7()); !errors.Is(err, ErrAPIKeyManagementRequired) {
			t.Fatalf("%s revoke error = %v", role, err)
		}
	}
	days, invalidDays := 90, 31
	for _, input := range []APIKeyInput{
		{Name: ""},
		{Name: "bad\nname"},
		{Name: string(make([]byte, 65))},
		{Name: "expiry", ExpiresInDays: &invalidDays},
	} {
		var inputError InputError
		if _, err := IssueAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, fixture.owner, fixture.scope, input); !errors.As(err, &inputError) {
			t.Fatalf("input %+v error = %v", input, err)
		}
	}
	expiring, err := IssueAPIKey(ctx, fixture.pool, auditpolicy.Policy{RetentionSeconds: 3600}, fixture.owner, fixture.scope, APIKeyInput{Name: "expiring", ExpiresInDays: &days})
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

type apiKeyCountingQuerier struct {
	db.DBTX
	statements atomic.Int64
}

// reset returns the statements counted since the last reset.
func (q *apiKeyCountingQuerier) reset() int64 {
	return q.statements.Swap(0)
}

func (q *apiKeyCountingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.statements.Add(1)
	return q.DBTX.QueryRow(ctx, sql, args...)
}
func (q *apiKeyCountingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.statements.Add(1)
	return q.DBTX.Query(ctx, sql, args...)
}

func TestAPIKeyEditsAndRotationAuditRollback(t *testing.T) {
	f := newAPIKeyFixture(t)
	ctx := t.Context()
	first := f.issue(t, "automation", auth.PermissionAgentsUse)
	id := pgvalue.MustUUIDValue(first.Record.ID)
	expires := time.Now().Add(time.Hour)
	edited, err := UpdateAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, id, APIKeyUpdate{Name: "renamed", ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	if edited.ID != first.Record.ID || !slices.Equal(edited.TokenHash, first.Record.TokenHash) {
		t.Fatal("policy edit changed identity/token")
	}
	var facts bool
	if err := f.pool.QueryRow(ctx, `SELECT details->'before'->>'name'='automation' AND details->'after'->>'name'='renamed' AND NOT (details->'after' ? 'permissions') FROM audit_events WHERE action='api_key.updated' AND target_id=$1`, id).Scan(&facts); err != nil || !facts {
		t.Fatal("audit facts", facts, err)
	}
	f.exec(t, `CREATE FUNCTION reject_key_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
	f.exec(t, `CREATE TRIGGER reject_key_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_key_audit()`)
	if _, err := RotateAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, id); err == nil {
		t.Fatal("rotation ignored audit failure")
	}
	if _, err := UpdateAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, id, APIKeyUpdate{Name: "lost"}); err == nil {
		t.Fatal("edit ignored audit failure")
	}
	if err := RevokeAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, id); err == nil {
		t.Fatal("revoke ignored audit failure")
	}
	current, err := NewAPIKeyAuthenticator(f.pool).Authenticate(ctx, first.Raw)
	if err != nil || !current.HasPermission(auth.PermissionAgentsUse, f.scope) {
		t.Fatal("rollback lost original active token or edited policy", err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&count); err != nil || count != 3 {
		t.Fatal("partial audit committed", count, err)
	}
}

func TestMachinePrincipalSurvivesCreatorOffboardingAndRejectsStaleManager(t *testing.T) {
	f := newAPIKeyFixture(t)
	ctx := t.Context()
	key := f.issue(t, "service", auth.PermissionAgentsUse)
	f.exec(t, `UPDATE org_members SET is_owner=false,removed_at=now() WHERE org_id=$1 AND user_id=$2`, f.owner.OrgID, f.owner.UserID)
	p, err := NewAPIKeyAuthenticator(f.pool).Authenticate(ctx, key.Raw)
	if err != nil || !p.HasPermission(auth.PermissionAgentsUse, f.scope) {
		t.Fatal("creator offboarding revoked machine identity", err)
	}
	if _, err := RotateAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, pgvalue.MustUUIDValue(key.Record.ID)); !errors.Is(err, ErrAPIKeyManagementRequired) {
		t.Fatal("stale manager rotated", err)
	}
}

func TestAPIKeyRotateRacesRevoke(t *testing.T) {
	f := newAPIKeyFixture(t)
	ctx := t.Context()
	adminID := f.user(t, "Other administrator", "")
	f.member(t, f.owner.OrgID, adminID, "admin")
	env := uuid.MustParse(f.scope.EnvironmentID)
	memberID := dbtest.GrantMember(t, f.pool, f.owner.OrgID, adminID, &env, "api_keys.read", "api_keys.create", "api_keys.update", "api_keys.rotate", "api_keys.revoke")
	admin := auth.Principal{OrgID: f.owner.OrgID, UserID: adminID, MemberID: memberID, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7(), Grants: f.owner.Grants}
	for i := 0; i < 4; i++ {
		key := f.issue(t, fmt.Sprintf("race-%d", i), auth.PermissionAgentsUse)
		id := pgvalue.MustUUIDValue(key.Record.ID)
		start := make(chan struct{})
		revoked := make(chan error, 1)
		type rotation struct {
			key IssuedAPIKey
			err error
		}
		rotated := make(chan rotation, 1)
		go func() {
			<-start
			k, err := RotateAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, admin, f.scope, id)
			rotated <- rotation{k, err}
		}()
		go func() {
			<-start
			revoked <- RevokeAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, id)
		}()
		close(start)
		if err := <-revoked; err != nil {
			t.Fatal(err)
		}
		result := <-rotated
		if result.err != nil && !errors.Is(result.err, ErrAPIKeyNotFound) {
			t.Fatal(result.err)
		}
		authenticator := NewAPIKeyAuthenticator(f.pool)
		if _, err := authenticator.Authenticate(ctx, key.Raw); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatal("old token survived revoke", err)
		}
		if result.err == nil {
			if _, err := authenticator.Authenticate(ctx, result.key.Raw); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatal("rotation resurrected revoked key", err)
			}
		}
		var rotations, revocations int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE action='api_key.rotated'),count(*) FILTER(WHERE action='api_key.revoked') FROM audit_events WHERE target_id=$1`, id).Scan(&rotations, &revocations); err != nil {
			t.Fatal(err)
		}
		want := 0
		if result.err == nil {
			want = 1
		}
		if rotations != want || revocations != 1 {
			t.Fatalf("audit facts rotations=%d revocations=%d", rotations, revocations)
		}
	}
}

func TestAPIKeyRoleIssuanceIsAtomicAndRequiresAccessManagement(t *testing.T) {
	f := newAPIKeyFixture(t)
	ctx := t.Context()
	role := f.role(t, auth.PermissionAgentsUse)
	f.exec(t, `DELETE FROM role_grants WHERE member_id=$1 AND environment_id IS NULL`, f.owner.MemberID)
	plain, err := IssueAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, APIKeyInput{Name: "unassigned"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewAPIKeyAuthenticator(f.pool).Authenticate(ctx, plain.Raw)
	if err != nil || len(p.Grants) != 0 || p.HasPermission(auth.PermissionAgentsUse, f.scope) {
		t.Fatal("unassigned key acquired access", err)
	}
	if _, err := IssueAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, APIKeyInput{Name: "cannot-assign", Role: role}); !errors.Is(err, org.ErrMemberManagementRequired) {
		t.Fatal("key manager assigned Role", err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE name='cannot-assign'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed assignment committed key", count, err)
	}
	dbtest.GrantMember(t, f.pool, f.owner.OrgID, f.owner.UserID, nil, "members.read", "members.remove", "invitations.read", "invitations.create", "invitations.revoke", "groups.read", "groups.create", "groups.update", "groups.delete", "groups.add_member", "groups.remove_member", "roles.read", "roles.create", "roles.update", "roles.delete", "assignments.read", "assignments.create", "assignments.update", "assignments.delete")
	role.ExpectedRoleRevision = 2
	if _, err := IssueAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, APIKeyInput{Name: "stale-role", Role: role}); !errors.Is(err, org.ErrStaleRevision) {
		t.Fatal(err)
	}
	role.ExpectedRoleRevision = 1
	key, err := IssueAPIKey(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.owner, f.scope, APIKeyInput{Name: "assigned", Role: role})
	if err != nil {
		t.Fatal(err)
	}
	p, err = NewAPIKeyAuthenticator(f.pool).Authenticate(ctx, key.Raw)
	if err != nil || !p.HasPermission(auth.PermissionAgentsUse, f.scope) {
		t.Fatal("Role not effective", err)
	}
}
