package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestPermissionManagementHTTP(t *testing.T) {
	f := newHTTPPostgresFixture(t)
	orgID, owner := f.organizationOwner(t, "permissions-http")
	_, stranger := f.organizationOwner(t, "other-permissions")
	user := f.user(t, "Member")
	memberID := f.member(t, orgID, user, "member")
	member := f.session(t, user, orgID)
	if _, err := f.queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: "permissions", DisplayName: "Permissions"}); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, token, revision string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if revision != "" {
			req.Header.Set("If-Match", `"`+revision+`"`)
		}
		out := httptest.NewRecorder()
		f.handler.ServeHTTP(out, req)
		return out
	}
	check := func(r *httptest.ResponseRecorder, status int, out any) {
		t.Helper()
		if r.Code != status {
			t.Fatalf("want %d got %d: %s", status, r.Code, r.Body.String())
		}
		if out != nil {
			if err := json.Unmarshal(r.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	body := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	check(request("POST", "/api/projects", `{"project":"work","name":"Work"}`, owner, ""), 201, nil)
	check(request("GET", "/api/projects/work/settings", "", member, ""), 404, nil)
	check(request("GET", "/api/projects/unknown/settings", "", member, ""), 404, nil)
	check(request("GET", "/api/projects/work/access-targets", "", member, ""), 403, nil)
	check(request("GET", "/api/projects/unknown/access-targets", "", member, ""), 403, nil)
	check(request("DELETE", "/api/members/"+uuid.NewV7().String(), "", owner, ""), 404, nil)
	var targets api.AccessTargetsResponse
	check(request("GET", "/api/projects/work/access-targets", "", owner, ""), 200, &targets)
	env := targets.Targets[0]
	agent, other := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'invoice'),($3,$2,'private')`, agent, env.EnvironmentID, other)
	var role api.PermissionRole
	check(request("POST", "/api/roles", `{"name":"User","description":"Original","permissions":["agents.use"]}`, owner, ""), 201, &role)
	var group api.PermissionGroup
	check(request("POST", "/api/groups", `{"name":"Team"}`, owner, ""), 201, &group)
	check(request("GET", "/api/roles/"+role.ID, "", stranger, ""), 404, nil)
	check(request("GET", "/api/roles/"+role.ID, "", member, ""), 403, nil)
	check(request("PUT", "/api/groups/"+group.ID+"/members/"+memberID.String(), "", owner, ""), 204, nil)
	check(request("PUT", "/api/groups/"+group.ID+"/members/"+memberID.String(), "", owner, ""), 204, nil)
	var members api.PermissionGroupMembersResponse
	check(request("GET", "/api/groups/"+group.ID+"/members", "", owner, ""), 200, &members)
	if len(members.MemberIDs) != 1 {
		t.Fatal(members)
	}
	input := api.RoleGrantInput{Subject: api.GrantSubject{Type: "group", GroupID: &group.ID}, RoleID: role.ID, ProjectID: &targets.ProjectID, EnvironmentID: &env.EnvironmentID, AgentsMode: "selected", AgentIDs: []string{agent.String()}, ExpectedRoleRevision: role.Revision}
	var grants api.RoleGrantsResponse
	check(request("POST", "/api/role-grants", body(input), owner, ""), 201, &grants)
	grant := grants.Grants[0]
	if !grant.Created {
		t.Fatal("creation not reported")
	}
	check(request("POST", "/api/role-grants", body(input), owner, ""), 200, &grants)
	if grants.Grants[0].ID != grant.ID || grants.Grants[0].Created {
		t.Fatal("retry not idempotent")
	}
	var auditCount int
	err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE org_id=$1 AND action='role_grant.created'`, orgID).Scan(&auditCount)
	if err != nil || auditCount != 1 {
		t.Fatalf("audit %d %v", auditCount, err)
	}
	// The grant that authorized the change is not the grant created by it.
	var distinct bool
	if err := f.pool.QueryRow(t.Context(), `SELECT actor_user_id IS NOT NULL AND authentication_result='verified' AND credential_kind='login_session' AND credential_id IS NOT NULL AND authorization_evidence->>'decision'='allowed' AND authorization_evidence->'checks' @> '[{"type":"permission","permission":"assignments.create","result":"passed"}]'::jsonb AND authorization_evidence->'checks'->0->'evidence'->>'assignment_id'<>target_id::text AND details->'before'='null'::jsonb AND details->'after'->>'role_id'=$2 AND details->'after'->>'group_id'=$3 AND details->'after'->'permissions'='["agents.use"]'::jsonb FROM audit_events WHERE action='role_grant.created' AND target_id=$1`, grant.ID, role.ID, group.ID).Scan(&distinct); err != nil || !distinct {
		t.Fatal("change and evaluated authority conflated", distinct, err)
	}
	// Concurrent duplicate creation converges on the existing tuple without extra audit events.
	var duplicateWG sync.WaitGroup
	duplicateCodes := make(chan int, 4)
	for range 4 {
		duplicateWG.Go(func() { duplicateCodes <- request("POST", "/api/role-grants", body(input), owner, "").Code })
	}
	duplicateWG.Wait()
	close(duplicateCodes)
	for code := range duplicateCodes {
		if code != 200 {
			t.Fatalf("duplicate assignment status %d", code)
		}
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE org_id=$1 AND action='role_grant.created'`, orgID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("duplicate audit %d %v", auditCount, err)
	}
	// Different POST targets conflict; conditional PATCH replaces the complete selection.
	input.AgentIDs = []string{agent.String(), other.String()}
	check(request("POST", "/api/role-grants", body(input), owner, ""), 409, nil)
	check(request("PATCH", "/api/role-grants/"+grant.ID, body(input), owner, ""), 428, nil)
	check(request("PATCH", "/api/role-grants/"+grant.ID, body(input), owner, strconv.FormatInt(grant.Revision, 10)), 200, &grant)
	check(request("DELETE", "/api/role-grants/"+grant.ID, "", owner, "1"), 412, nil)
	input.AgentIDs = []string{uuid.NewV7().String(), agent.String()}
	check(request("PATCH", "/api/role-grants/"+grant.ID, body(input), owner, strconv.FormatInt(grant.Revision, 10)), 404, nil)
	check(request("GET", "/api/role-grants?limit=1", "", owner, ""), 200, &grants)
	if len(grants.Grants) != 1 || grants.NextCursor == "" {
		t.Fatal(grants)
	}
	first := grants.Grants[0].ID
	check(request("GET", "/api/role-grants?limit=1&cursor="+grants.NextCursor, "", owner, ""), 200, &grants)
	if len(grants.Grants) != 1 || grants.Grants[0].ID == first {
		t.Fatal(grants)
	}
	// PATCH preserves omitted fields, requires a revision, and rejects stale edits.
	check(request("PATCH", "/api/roles/"+role.ID, `{"name":"Renamed"}`, owner, ""), 428, nil)
	oldRevision := role.Revision
	check(request("PATCH", "/api/roles/"+role.ID, `{"name":"Renamed"}`, owner, strconv.FormatInt(oldRevision, 10)), 200, &role)
	if role.Description != "Original" || len(role.Permissions) != 1 || role.Revision == oldRevision {
		t.Fatal(role)
	}
	check(request("PATCH", "/api/roles/"+role.ID, `{"permissions":["sessions.send"]}`, owner, strconv.FormatInt(oldRevision, 10)), 412, nil)
	for _, conflict := range []struct {
		method, body, code string
		status             int
	}{
		{"PATCH", `{"permissions":["assignments.create"]}`, "role_target_incompatible", 409},
		{"DELETE", "", "role_in_use", 409},
	} {
		var response struct {
			Error struct {
				Code    string
				Details struct {
					GrantIDs []string `json:"grant_ids"`
				}
			}
		}
		check(request(conflict.method, "/api/roles/"+role.ID, conflict.body, owner, strconv.FormatInt(role.Revision, 10)), conflict.status, &response)
		if response.Error.Code != conflict.code || len(response.Error.Details.GrantIDs) != 1 {
			t.Fatalf("conflict details: %+v", response)
		}
	}
	// One of two same-revision edits wins.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := request("PATCH", "/api/roles/"+role.ID, fmt.Sprintf(`{"description":"Edit %d"}`, i), owner, strconv.FormatInt(role.Revision, 10))
			codes <- r.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[200] != 1 || counts[412] != 1 {
		t.Fatal(counts)
	}
	// Unrelated members cannot inspect another member, and workload keys cannot use this surface.
	outsider := f.user(t, "Outsider")
	outsiderMemberID := f.member(t, orgID, outsider, "member")
	outsiderToken := f.session(t, outsider, orgID)
	path := "/api/members/" + memberID.String() + "/permissions?project=work&environment=" + env.Environment
	check(request("GET", path, "", outsiderToken, ""), 403, nil)
	var effective api.MemberPermissionsResponse
	check(request("GET", path, "", member, ""), 200, &effective)
	if len(effective.Scopes) != 2 {
		t.Fatalf("scopes %+v", effective)
	}
	for _, scope := range effective.Scopes {
		if scope.Agent == "" && len(scope.Permissions) != 0 {
			t.Fatal("Agent grant leaked into environment scope")
		}
	}
	check(request("GET", path+"&agent=invoice", "", member, ""), 200, &effective)
	if len(effective.Scopes) != 1 || len(effective.Scopes[0].Permissions) != 1 || len(effective.Scopes[0].Sources) != 1 {
		t.Fatal(effective)
	}
	source := effective.Scopes[0].Sources[0]
	if source.TargetKind != "agent" || source.Agent != "invoice" || source.AgentID != agent.String() || len(source.Permissions) != 1 {
		t.Fatalf("source target: %+v", source)
	}
	check(request("GET", "/api/members/"+outsiderMemberID.String()+"/permissions?project=work&environment="+env.Environment+"&agent=invoice", "", outsiderToken, ""), 404, nil)
	check(request("GET", "/api/members/"+memberID.String()+"/permissions", "", stranger, ""), 404, nil)
	check(request("PUT", "/api/groups/"+group.ID+"/members/"+outsiderMemberID.String(), "", owner, ""), 204, nil)
	check(request("GET", "/api/groups/"+group.ID+"/members?limit=1", "", owner, ""), 200, &members)
	if len(members.MemberIDs) != 1 || members.NextCursor == "" {
		t.Fatal(members)
	}
	firstMember, memberCursor := members.MemberIDs[0], members.NextCursor
	members = api.PermissionGroupMembersResponse{}
	check(request("GET", "/api/groups/"+group.ID+"/members?limit=1&cursor="+memberCursor, "", owner, ""), 200, &members)
	if len(members.MemberIDs) != 1 || members.MemberIDs[0] == firstMember || members.NextCursor != "" {
		t.Fatal(members)
	}
	// Removing one path preserves an independent All Users grant.
	input.Subject = api.GrantSubject{Type: "all_members"}
	input.AgentIDs = []string{agent.String()}
	check(request("GET", "/api/roles/"+role.ID, "", owner, ""), 200, &role)
	input.ExpectedRoleRevision = role.Revision
	check(request("POST", "/api/role-grants", body(input), owner, ""), 201, nil)
	check(request("DELETE", "/api/role-grants/"+grant.ID, "", owner, strconv.FormatInt(grant.Revision, 10)), 204, nil)
	check(request("GET", path+"&agent=invoice", "", member, ""), 200, &effective)
	if len(effective.Scopes[0].Permissions) != 1 || effective.Scopes[0].Sources[0].SubjectKind != "all_members" {
		t.Fatal(effective)
	}
	// An Agent projection preserves the breadth and implied capabilities of
	// Environment grants alongside an exact-Agent grant.
	var operator api.PermissionRole
	check(request("POST", "/api/roles", `{"name":"Operator","permissions":["sessions.send"]}`, owner, ""), 201, &operator)
	input.Subject = api.GrantSubject{Type: "group", GroupID: &group.ID}
	input.RoleID = operator.ID
	input.AgentsMode = "all"
	input.AgentIDs = nil
	input.ExpectedRoleRevision = operator.Revision
	check(request("POST", "/api/role-grants", body(input), owner, ""), 201, nil)
	check(request("GET", path+"&agent=invoice", "", member, ""), 200, &effective)
	if len(effective.Scopes[0].Sources) != 2 {
		t.Fatal(effective)
	}
	for _, source := range effective.Scopes[0].Sources {
		if source.RoleID == operator.ID && (source.TargetKind != "environment" || source.AgentID != "" || len(source.Permissions) != 1) {
			t.Fatalf("environment source: %+v", source)
		}
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE org_id=$1 AND user_id=$2`, orgID, user)
	check(request("GET", path, "", owner, ""), 200, &effective)
	if effective.Status != "removed" || len(effective.Scopes) != 0 {
		t.Fatal(effective)
	}
}
