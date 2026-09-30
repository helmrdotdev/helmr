package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestComputerMembersReadAuthoritativeRowsAndScopeCursor(t *testing.T) {
	f := newActorStartPostgresFixture(t, 2)
	started, err := f.server.startActor(t.Context(), f.request(0, nil, "member-session"))
	if err != nil {
		t.Fatal(err)
	}
	commandID, claimID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO idempotency_claims
 (id,environment_id,operation,slot_hash,request_fingerprint,status,receipt,accepted_at,completed_at,receipt_expires_at)
 VALUES($1,$2,'computer.exec',$3,$3,'completed',jsonb_build_object('command_id',$4::text),now(),now(),now()+interval '30 days')`,
		claimID, f.environmentID, dbtest.Hash(commandID.String()), commandID.String())
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_commands
 (id,environment_id,computer_id,argv,cwd,env,stdin,timeout_ms,claim_id,created_by_subject_type,created_by_subject_id)
 SELECT $1,environment_id,id,ARRAY['true'],'/workspace','{}'::jsonb,''::bytea,300000,$3,'user','test'
 FROM computers WHERE id=$2`, commandID, f.computerIDs[0], claimID)
	principal := auth.Principal{
		OrgID: f.orgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: f.projectID.String(), EnvironmentID: f.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionComputersRead},
	}
	read := func(id uuid.UUID, query string, principal auth.Principal, want int) api.ListComputerMembersResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		f.server.listComputerMembersHTTP(recorder, computerReadPostgresRequest("/v1/computers/"+id.String()+"/members"+query, id.String(), principal))
		if recorder.Code != want {
			t.Fatalf("members status=%d want=%d: %s", recorder.Code, want, recorder.Body.String())
		}
		var response api.ListComputerMembersResponse
		if want == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return response
	}
	first := read(f.computerIDs[0], "?limit=1", principal, http.StatusOK)
	if len(first.Members) != 1 || first.Members[0].Kind != "command" || first.Members[0].ID != commandID.String() || first.Members[0].State != "admitted" || first.Members[0].RunID != "" || first.NextCursor == "" {
		t.Fatalf("first page=%+v", first)
	}
	second := read(f.computerIDs[0], "?limit=1&cursor="+url.QueryEscape(first.NextCursor), principal, http.StatusOK)
	if len(second.Members) != 1 || second.Members[0].Kind != "session" || second.Members[0].ID != started.SessionID.String() || second.NextCursor != "" || second.Members[0].State != "admitted" {
		t.Fatalf("second page=%+v", second)
	}
	read(f.computerIDs[1], "?cursor="+url.QueryEscape(first.NextCursor), principal, http.StatusBadRequest)
	read(uuid.NewV7(), "", principal, http.StatusNotFound)
	unprivileged := principal
	unprivileged.Permissions = nil
	read(f.computerIDs[0], "", unprivileged, http.StatusForbidden)
	wrongEnvironment := principal
	wrongEnvironment.EnvironmentID = uuid.NewV7().String()
	read(f.computerIDs[0], "", wrongEnvironment, http.StatusNotFound)
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=abc", "?limit=1&limit=2", "?key=test", "?cursor=broken"} {
		read(f.computerIDs[0], query, principal, http.StatusBadRequest)
	}
	invalidLimit := httptest.NewRecorder()
	f.server.listComputerMembersHTTP(invalidLimit, computerReadPostgresRequest("/v1/computers/"+f.computerIDs[0].String()+"/members?limit=abc", f.computerIDs[0].String(), principal))
	if body := decodeHTTPError(t, invalidLimit.Body.Bytes()); body.Code != "invalid_computer_reference" || body.Message != "limit must be an integer in [1,100]" {
		t.Fatalf("invalid limit error = %+v", body)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_commands SET status='failed',failure_reason='placement_failed',terminal_at=now(),terminal_reason_code='placement_failed' WHERE id=$1`, commandID)
	last := read(f.computerIDs[0], "", principal, http.StatusOK)
	if len(last.Members) != 1 || last.Members[0].ID != started.SessionID.String() {
		t.Fatalf("settled membership=%+v", last)
	}
}
