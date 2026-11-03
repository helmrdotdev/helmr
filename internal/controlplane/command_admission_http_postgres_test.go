package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestCommandHTTPAdmissionCapacity(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	f.SetPermissions(t, "api_keys.create", "computers.execute", "computers.read")
	issuer := f.Principal(t)
	var project uuid.UUID
	if err := f.Pool.QueryRow(ctx, "SELECT project_id FROM environments WHERE id=$1", f.Environment).Scan(&project); err != nil {
		t.Fatal(err)
	}
	key := func(permission auth.Permission) string {
		t.Helper()
		issued, err := issueAPIKeyWithRole(t, ctx, f.Pool, issuer, auth.Scope{OrgID: issuer.OrgID, ProjectID: project.String(), EnvironmentID: f.Environment.String()}, testAPIKeyInput{Name: string(permission), RolePermissions: []auth.Permission{permission}})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Raw
	}
	executor, reader := key(auth.PermissionComputersExecute), key(auth.PermissionComputersRead)

	// A second Computer has a ready root, but the fixture Worker holds the only slot.
	id := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes)
 SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes FROM computers WHERE id=$1`, f.Computer, id)
	client := httpPostgresFixture{handler: newPostgresServer(t, f.Pool)}
	post := func(token string, status int, code string) []byte {
		t.Helper()
		resp := client.request(t, http.MethodPost, "/v1/computers/"+id.String()+"/exec", token, `{"command":["true"],"idempotency_key":"first"}`)
		if resp.Code != status {
			t.Fatalf("status %d, want %d: %s", resp.Code, status, resp.Body.String())
		}
		if code != "" && decodeHTTPError(t, resp.Body.Bytes()).Code != code {
			t.Fatalf("want %s: %s", code, resp.Body.String())
		}
		return resp.Body.Bytes()
	}
	post(reader, http.StatusForbidden, "permission_required")
	post(executor, http.StatusServiceUnavailable, "execution_capacity_unavailable")
	sessiontest.ObserveProvisioning(t, f)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE worker_groups SET capacity_pools=jsonb_set(capacity_pools,'{0,max_additional_workers}','0') WHERE id=$1`, f.Group)
	post(executor, http.StatusConflict, "execution_capacity_exhausted")
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET cpu_millis=2000 WHERE id=$1`, id)
	post(executor, http.StatusConflict, "execution_capacity_incompatible")
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET cpu_millis=1000 WHERE id=$1`, id)
	sessiontest.ObserveProvisioning(t, f)
	var first, replay api.CommandReceipt
	if err := json.Unmarshal(post(executor, http.StatusAccepted, ""), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(post(executor, http.StatusAccepted, ""), &replay); err != nil || first.CommandID == "" || first != replay {
		t.Fatalf("receipts: %+v %+v %v", first, replay, err)
	}
}
