package controlplane

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerMembersReadAuthoritativeRowsAndScopeCursor(t *testing.T) {
	f := sessiontest.New(t, 2)
	started := startSession(t, f, 0, nil, "member-session")
	commandID, claimID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims
 (id,environment_id,operation,slot_hash,request_fingerprint,status,receipt,accepted_at,completed_at,receipt_expires_at)
 VALUES($1,$2,'computer.exec',$3,$3,'completed',jsonb_build_object('command_id',$4::text),now(),now(),now()+interval '30 days')`,
		claimID, f.EnvironmentID, dbtest.Hash(commandID.String()), commandID.String())
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands
 (id,environment_id,computer_id,argv,cwd,env,stdin,timeout_ms,claim_id,created_by_subject_type,created_by_subject_id)
 SELECT $1,environment_id,id,ARRAY['true'],'/workspace','{}'::jsonb,''::bytea,300000,$3,'user','test'
 FROM computers WHERE id=$2`, commandID, f.ComputerIDs[0], claimID)
	handler := newPostgresServer(t, f.Pool)
	reader := issueEnvironmentAPIKey(t, f.Pool, f.OrgID, f.ProjectID, f.EnvironmentID, auth.PermissionComputersRead)
	read := func(id uuid.UUID, query string, key string, want int) api.ListComputerMembersResponse {
		t.Helper()
		recorder := serveAPIKey(handler, http.MethodGet, "/v1/computers/"+id.String()+"/members"+query, key, "")
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
	first := read(f.ComputerIDs[0], "?limit=1", reader, http.StatusOK)
	if len(first.Members) != 1 || first.Members[0].Kind != "command" || first.Members[0].ID != commandID.String() || first.Members[0].State != "admitted" || first.Members[0].RunID != "" || first.NextCursor == "" {
		t.Fatalf("first page=%+v", first)
	}
	second := read(f.ComputerIDs[0], "?limit=1&cursor="+url.QueryEscape(first.NextCursor), reader, http.StatusOK)
	if len(second.Members) != 1 || second.Members[0].Kind != "session" || second.Members[0].ID != started.SessionID.String() || second.NextCursor != "" || second.Members[0].State != "admitted" {
		t.Fatalf("second page=%+v", second)
	}
	read(f.ComputerIDs[1], "?cursor="+url.QueryEscape(first.NextCursor), reader, http.StatusBadRequest)
	read(uuid.NewV7(), "", reader, http.StatusNotFound)
	unprivileged := issueEnvironmentAPIKey(t, f.Pool, f.OrgID, f.ProjectID, f.EnvironmentID, auth.PermissionRunsRead)
	read(f.ComputerIDs[0], "", unprivileged, http.StatusForbidden)
	otherEnvironment := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
 VALUES ($1, $2, $3, $4, 'Other', '#3366ff')`, otherEnvironment, f.OrgID, f.ProjectID, "other-"+otherEnvironment.String())
	read(f.ComputerIDs[0], "", issueEnvironmentAPIKey(t, f.Pool, f.OrgID, f.ProjectID, otherEnvironment, auth.PermissionComputersRead), http.StatusNotFound)
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=abc", "?limit=1&limit=2", "?key=test", "?cursor=broken"} {
		read(f.ComputerIDs[0], query, reader, http.StatusBadRequest)
	}
	invalidLimit := serveAPIKey(handler, http.MethodGet, "/v1/computers/"+f.ComputerIDs[0].String()+"/members?limit=abc", reader, "")
	if body := decodeHTTPError(t, invalidLimit.Body.Bytes()); body.Code != "invalid_computer_reference" || body.Message != "limit must be an integer in [1,100]" {
		t.Fatalf("invalid limit error = %+v", body)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='failed',failure_reason='dispatch_failed',terminal_at=now(),terminal_reason_code='dispatch_failed' WHERE id=$1`, commandID)
	last := read(f.ComputerIDs[0], "", reader, http.StatusOK)
	if len(last.Members) != 1 || last.Members[0].ID != started.SessionID.String() {
		t.Fatalf("settled membership=%+v", last)
	}
}
