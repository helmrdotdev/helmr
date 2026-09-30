package controlplane

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestCapacityHTTPRequiresDedicatedBearer(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	for name, authorization := range map[string]string{
		"missing":         "",
		"product token":   "hlmr_test_product",
		"session token":   admin,
		"malformed token": "Basic " + capacityTestToken(),
	} {
		t.Run(name, func(t *testing.T) {
			response := f.request(t, http.MethodGet, "/capacity/v1/worker-hosts", authorization, "")
			if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("status = %d, WWW-Authenticate = %q: %s", response.Code, response.Header().Get("WWW-Authenticate"), response.Body.String())
			}
		})
	}
}

func TestCapacityHTTPPreservesRequestBodyLimits(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	for _, test := range []struct {
		name   string
		path   string
		length int64
	}{
		{name: "common limit", path: "/capacity/v1/worker-hosts/01900000-0000-7000-8000-000000000000/lost", length: apiRequestBodyLimit + 1},
		{name: "mutation limit", path: "/capacity/v1/worker-groups/01900000-0000-7000-8000-000000000000/plan", length: capacityRequestBodyLimit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("x"))
			request.ContentLength = test.length
			request.Header.Set("Authorization", "Bearer "+capacityTestToken())
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, request)
			assertAdminError(t, response, http.StatusRequestEntityTooLarge, "request_too_large")
		})
	}
}

func TestCapacityHTTPResolveAndPlan(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	group := f.supplyGroup(t, admin, "aws-us-east-1")
	token := capacityTestToken()
	pool := decodeAdmin[struct {
		ID string `json:"id"`
	}](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/pools", admin,
		fmt.Sprintf(`{"name":"run-current","expected_group_claim_version":%d}`, group.ClaimVersion)), http.StatusCreated)
	f.sealWorkerPool(t, group.ID, pool.ID)

	resolved := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-groups/resolve?region_id=aws-us-east-1&name=default", token, ""), http.StatusOK)
	assertCapacityJSONKeys(t, resolved, "claim_version", "id", "name", "region_id", "status")
	if resolved["id"] != group.ID || resolved["status"] != "active" {
		t.Fatalf("resolved group = %#v", resolved)
	}
	resolvedPool := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-groups/"+group.ID+"/pools/resolve?name=run-current", token, ""), http.StatusOK)
	assertCapacityJSONKeys(t, resolvedPool, "id", "name", "status", "worker_group_id")
	if resolvedPool["id"] != pool.ID || resolvedPool["status"] != "active" {
		t.Fatalf("resolved pool = %#v", resolvedPool)
	}
	assertAdminError(t, f.request(t, http.MethodGet, "/capacity/v1/worker-groups/resolve?region_id=aws-us-east-1&name=missing", token, ""), http.StatusNotFound, "not_found")
	assertAdminError(t, f.request(t, http.MethodGet, "/capacity/v1/worker-groups/"+group.ID+"/pools/resolve?name=missing", token, ""), http.StatusNotFound, "not_found")

	plan := capacityJSON(t, f.request(t, http.MethodPost, "/capacity/v1/worker-groups/"+group.ID+"/plan", token,
		fmt.Sprintf(`{"pools":[{"pool_id":%q,"max_additional_workers":2}]}`, pool.ID)), http.StatusOK)
	assertCapacityJSONKeys(t, plan, "complete", "computed_at", "group_status", "pools", "region_id", "unmatched_demand", "worker_group_id", "worker_group_name")
	pools, ok := plan["pools"].([]any)
	if !ok || len(pools) != 1 {
		t.Fatalf("plan pools = %#v", plan["pools"])
	}
	poolPlan := capacityJSONObject(t, pools[0])
	assertCapacityJSONKeys(t, poolPlan, "active_workers", "complete", "compatible_queued_items", "pool_id", "pool_name", "recommended_additional_workers", "registering_workers", "saturated", "scale_in_blocked")
	if plan["worker_group_id"] != group.ID || plan["complete"] != true || poolPlan["recommended_additional_workers"] != float64(0) {
		t.Fatalf("plan = %#v", plan)
	}
	assertAdminError(t, f.request(t, http.MethodPost, "/capacity/v1/worker-groups/"+uuid.NewV7().String()+"/plan", token,
		fmt.Sprintf(`{"pools":[{"pool_id":%q,"max_additional_workers":2}]}`, pool.ID)), http.StatusNotFound, "not_found")

	primaries := capacityJSON(t, f.request(t, http.MethodPut, "/capacity/v1/worker-groups/"+group.ID+"/primary-pools", token,
		fmt.Sprintf(`{"expected_group_claim_version":%d,"pool_id":%q}`, group.ClaimVersion, pool.ID)), http.StatusOK)
	assertCapacityJSONKeys(t, primaries, "applied", "worker_group")
	if primaries["applied"] != true || capacityJSONObject(t, primaries["worker_group"])["primary_pool_id"] != pool.ID {
		t.Fatalf("primary pools = %#v", primaries)
	}
	assertAdminError(t, f.request(t, http.MethodPut, "/capacity/v1/worker-groups/"+group.ID+"/primary-pools", token,
		fmt.Sprintf(`{"expected_group_claim_version":%d,"pool_id":""}`, group.ClaimVersion+1)), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.request(t, http.MethodPut, "/capacity/v1/worker-groups/"+uuid.NewV7().String()+"/primary-pools", token,
		`{"expected_group_claim_version":1,"pool_id":""}`), http.StatusNotFound, "not_found")

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/capacity/v1/worker-groups/resolve?region_id=aws-us-east-1&region_id=other&name=default"},
		{method: http.MethodGet, path: "/capacity/v1/worker-hosts?worker_group_id=not-a-group"},
		{method: http.MethodGet, path: "/capacity/v1/worker-hosts?worker_group_id=%20"},
		{method: http.MethodGet, path: "/capacity/v1/worker-hosts?worker_group_id=%20" + group.ID + "%20"},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/not-a-group/plan", body: `{}`},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/" + group.ID + "/plan", body: `{}`},
		{method: http.MethodPut, path: "/capacity/v1/worker-groups/" + group.ID + "/primary-pools", body: `{"expected_group_claim_version":0,"pool_id":""}`},
	} {
		assertAdminError(t, f.request(t, test.method, test.path, token, test.body), http.StatusBadRequest, "bad_request")
	}
}

func TestCapacityHTTPWorkerHosts(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	group := f.supplyGroup(t, admin, "aws-us-east-1")
	token := capacityTestToken()
	pool := decodeAdmin[struct {
		ID string `json:"id"`
	}](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/pools", admin,
		fmt.Sprintf(`{"name":"run-current","expected_group_claim_version":%d}`, group.ClaimVersion)), http.StatusCreated)
	f.sealWorkerPool(t, group.ID, pool.ID)
	hostID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `
INSERT INTO worker_hosts (
    id, resource_id, worker_group_id, worker_pool_id, status, claim_version,
    current_epoch, current_service_id, vm_platform_id,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, max_vm_starts,
    cpu_environment, cpu_environment_digest, observed_at,
    epoch_started_at, activated_at
) VALUES (
    $1, 'host-opaque-1', $2, $3, 'active', 7,
    4, $4, $5,
    4000, 8589934592, 34359738368,
    1000, 1073741824, 4294967296,
    4, 4, '{"vendor":"test"}'::jsonb, $6, now(), now(), now()
)`, hostID, group.ID, pool.ID, uuid.NewV7(), dbtest.Digest("supply-http-runtime"), dbtest.Digest("supply-http-cpu-environment"))

	list := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-hosts?worker_group_id="+group.ID+"&status=active&limit=1", token, ""), http.StatusOK)
	assertCapacityJSONKeys(t, list, "worker_hosts")
	hosts, ok := list["worker_hosts"].([]any)
	if !ok || len(hosts) != 1 {
		t.Fatalf("worker_hosts = %#v", list["worker_hosts"])
	}
	listed := capacityJSONObject(t, hosts[0])
	assertCapacityJSONKeys(t, listed, "claim_version", "created_at", "current_epoch", "id", "resource_id", "status", "updated_at", "worker_group_id", "worker_pool_id")
	if listed["id"] != hostID.String() || listed["resource_id"] != "host-opaque-1" ||
		listed["worker_group_id"] != group.ID || listed["worker_pool_id"] != pool.ID ||
		listed["status"] != "active" || listed["claim_version"] != float64(7) || listed["current_epoch"] != float64(4) {
		t.Fatalf("listed host = %#v", listed)
	}
	if empty := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-hosts?status=lost", token, ""), http.StatusOK); len(empty["worker_hosts"].([]any)) != 0 {
		t.Fatalf("lost hosts = %#v", empty)
	}
	got := capacityJSON(t, f.request(t, http.MethodGet, "/capacity/v1/worker-hosts/"+hostID.String(), token, ""), http.StatusOK)
	if !reflect.DeepEqual(got, listed) {
		t.Fatalf("get = %#v, want listed host %#v", got, listed)
	}
	assertAdminError(t, f.request(t, http.MethodGet, "/capacity/v1/worker-hosts/"+uuid.NewV7().String(), token, ""), http.StatusNotFound, "not_found")

	drainPath := "/capacity/v1/worker-hosts/" + hostID.String() + "/drain"
	assertAdminError(t, f.request(t, http.MethodPost, drainPath, token, `{"expected_epoch":0,"expected_claim_version":7}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.request(t, http.MethodPost, drainPath, token, `{"expected_epoch":4,"expected_claim_version":6}`), http.StatusConflict, "conflict")
	drained := capacityJSON(t, f.request(t, http.MethodPost, drainPath, token, `{"expected_epoch":4,"expected_claim_version":7,"require_zero_queued_demand":true}`), http.StatusOK)
	if drained["status"] != "draining" || drained["claim_version"] != float64(8) || drained["draining_at"] == nil {
		t.Fatalf("drained host = %#v", drained)
	}

	lostPath := "/capacity/v1/worker-hosts/" + hostID.String() + "/lost"
	assertAdminError(t, f.request(t, http.MethodPost, lostPath, token, `{}`), http.StatusBadRequest, "bad_request")
	lost := capacityJSON(t, f.request(t, http.MethodPost, lostPath, token, ""), http.StatusOK)
	if lost["status"] != "lost" || lost["lost_at"] == nil {
		t.Fatalf("lost host = %#v", lost)
	}
	assertAdminError(t, f.request(t, http.MethodPost, "/capacity/v1/worker-hosts/"+uuid.NewV7().String()+"/lost", token, ""), http.StatusNotFound, "not_found")
}
