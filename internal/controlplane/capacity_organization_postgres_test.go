package controlplane

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// organizationSupply creates an active organization group named like the
// deployment group in its region, with a sealed pool and one active host.
func (f httpPostgresFixture) organizationSupply(t *testing.T, admin, regionID, name string) (group, pool, host string) {
	t.Helper()
	org := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO organizations (id, name, slug) VALUES ($1, 'Org', $2)`, org, org.String())
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.queries.CreateWorkerGroup(t.Context(), db.CreateWorkerGroupParams{
		ID: pgvalue.NewUUIDv7(), OwnerOrgID: pgvalue.UUID(org), TokenID: pgvalue.NewUUIDv7(), TokenHash: token.Hash, RegionID: regionID, Name: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	group = pgvalue.UUIDString(created.ID)
	pool = decodeAdmin[struct {
		ID string `json:"id"`
	}](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group+"/pools", admin,
		fmt.Sprintf(`{"name":"run-current","expected_group_claim_version":%d}`, created.ClaimVersion)), http.StatusCreated).ID
	f.sealWorkerPool(t, group, pool)
	f.readyWorkerHost(t, group, pool)
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM worker_hosts WHERE worker_group_id=$1`, group).Scan(&host); err != nil {
		t.Fatal(err)
	}
	return group, pool, host
}

func TestCapacityHTTPRefusesOrganizationCapacity(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	token := capacityTestToken()
	deployment := f.supplyGroup(t, admin, "aws-us-east-1")
	deploymentPool := decodeAdmin[struct {
		ID string `json:"id"`
	}](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+deployment.ID+"/pools", admin,
		fmt.Sprintf(`{"name":"run-current","expected_group_claim_version":%d}`, deployment.ClaimVersion)), http.StatusCreated).ID
	f.sealWorkerPool(t, deployment.ID, deploymentPool)
	f.readyWorkerHost(t, deployment.ID, deploymentPool)
	group, pool, host := f.organizationSupply(t, admin, "aws-us-east-1", "default")

	state := func() string {
		var snapshot string
		if err := f.pool.QueryRow(t.Context(), `SELECT jsonb_build_array(
 (SELECT to_jsonb(g) FROM worker_groups g WHERE g.id=$1),
 (SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM worker_pools p WHERE p.worker_group_id=$1),
 (SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM worker_hosts h WHERE h.worker_group_id=$1),
 (SELECT count(*) FROM worker_scale_in_operations o WHERE o.worker_group_id=$1),
 (SELECT count(*) FROM audit_events))::text`, group).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := state()

	resolved := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-groups/resolve?region_id=aws-us-east-1&name=default", token, ""), http.StatusOK)
	if resolved["id"] != deployment.ID {
		t.Fatalf("resolved group = %#v, want the deployment group %s", resolved, deployment.ID)
	}

	// Every route answers for an organization group or host exactly as for an
	// ID that does not exist.
	missingGroup, missingHost := uuid.NewV7().String(), uuid.NewV7().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, route := range []struct {
		method, path, body string
	}{
		{method: http.MethodGet, path: "/capacity/v1/worker-groups/{group}/pools/resolve?name=run-current"},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/{group}/pools", body: `{"name":"run-next","expected_group_claim_version":2}`},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/{group}/pools/{pool}/disable", body: `{"expected_pool_claim_version":1}`},
		{method: http.MethodPut, path: "/capacity/v1/worker-groups/{group}/primary-pool", body: `{"expected_group_claim_version":2,"pool_id":"{pool}","minimum_ready_hosts":1}`},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/{group}/plan", body: `{"expected_supply_fingerprint":"x","observed_at":"` + now + `","pools":[{"pool_id":"{pool}","max_additional_workers":2}]}`},
		{method: http.MethodGet, path: "/capacity/v1/worker-groups/{group}/pools/{pool}/resource-policy"},
		{method: http.MethodPut, path: "/capacity/v1/worker-groups/{group}/pools/{pool}/resource-policy", body: `{"expected_revision":1,"optional":{"cpu_millis":0,"memory_bytes":0,"disk_bytes":0,"vm_slots":0,"max_tasks":0,"io_bytes_per_second":0},"local_headroom_slots":0}`},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/{group}/pools/{pool}/scale-in", body: `{"operation_key":"` + uuid.NewV7().String() + `"}`},
		{method: http.MethodGet, path: "/capacity/v1/worker-groups/{group}/scale-in-operations/pending"},
		{method: http.MethodGet, path: "/capacity/v1/worker-groups/{group}/scale-in-operations/" + uuid.NewV7().String()},
		{method: http.MethodGet, path: "/capacity/v1/worker-hosts/{host}"},
		{method: http.MethodPost, path: "/capacity/v1/worker-hosts/{host}/drain", body: `{"expected_epoch":1,"expected_claim_version":1,"reason":"replacement"}`},
		{method: http.MethodPost, path: "/capacity/v1/worker-hosts/{host}/lost"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			bind := func(text, group, host string) string {
				return strings.NewReplacer("{group}", group, "{pool}", pool, "{host}", host).Replace(text)
			}
			foreign := f.request(t, route.method, bind(route.path, group, host), token, bind(route.body, group, host))
			missing := f.request(t, route.method, bind(route.path, missingGroup, missingHost), token, bind(route.body, missingGroup, missingHost))
			assertAdminError(t, foreign, http.StatusNotFound, "not_found")
			if foreign.Body.String() != missing.Body.String() || missing.Code != foreign.Code {
				t.Fatalf("organization response %d %s differs from missing %d %s", foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
			}
		})
	}

	listed := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-hosts", token, ""), http.StatusOK)["worker_hosts"].([]any)
	if len(listed) != 1 || capacityJSONObject(t, listed[0])["worker_group_id"] != deployment.ID {
		t.Fatalf("listed hosts = %#v, want only the deployment host", listed)
	}
	for _, query := range []string{"worker_group_id=" + group, "resource_id=" + host, "worker_group_id=" + group + "&status=active&has_unreclaimed_instance=true"} {
		if hosts := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-hosts?"+query, token, ""), http.StatusOK)["worker_hosts"].([]any); len(hosts) != 0 {
			t.Fatalf("hosts for %s = %#v", query, hosts)
		}
	}
	if after := state(); after != before {
		t.Fatalf("capacity requests changed organization supply:\nbefore %s\nafter  %s", before, after)
	}
}
