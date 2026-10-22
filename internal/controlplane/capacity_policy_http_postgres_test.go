package controlplane

import (
	"fmt"
	"net/http"
	"testing"
	"uuid"
)

func TestCapacityHTTPResourcePolicyAndScaleInReceipts(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	group := f.supplyGroup(t, admin, "aws-us-east-1")
	token := capacityTestToken()
	pool := decodeAdmin[struct {
		ID string `json:"id"`
	}](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/pools", admin, fmt.Sprintf(`{"name":"policy","expected_group_claim_version":%d}`, group.ClaimVersion)), http.StatusCreated)
	f.sealWorkerPool(t, group.ID, pool.ID)
	path := "/capacity/v1/worker-groups/" + group.ID + "/pools/" + pool.ID
	initial := capacityJSON(t, f.request(t, http.MethodGet, path+"/resource-policy", token, ""), http.StatusOK)
	if initial["revision"] != float64(1) {
		t.Fatalf("initial policy: %#v", initial)
	}
	body := `{"expected_revision":1,"optional":{"cpu_millis":1000,"memory_bytes":1073741824,"disk_bytes":1073741824,"vm_slots":1,"max_tasks":1,"io_bytes_per_second":1048576},"local_headroom_slots":0}`
	updated := capacityJSON(t, f.request(t, http.MethodPut, path+"/resource-policy", token, body), http.StatusOK)
	if updated["revision"] != float64(2) {
		t.Fatalf("updated policy: %#v", updated)
	}
	assertAdminError(t, f.request(t, http.MethodPut, path+"/resource-policy", token, body), http.StatusConflict, "resource_policy_revision_conflict")
	assertAdminError(t, f.request(t, http.MethodPut, path+"/resource-policy", token, `{"expected_revision":2,"optional":{},"local_headroom_slots":0}`), http.StatusBadRequest, "invalid_resource_policy")
	if response := f.request(t, http.MethodGet, path+"/resource-policy", admin, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("product authority accessed operator policy: %d", response.Code)
	}
	pendingPath := "/capacity/v1/worker-groups/" + group.ID + "/scale-in-operations/"
	if got := capacityJSON(t, f.request(t, http.MethodGet, pendingPath+"pending", token, ""), http.StatusOK); got["operation"] != nil {
		t.Fatalf("unexpected pending: %#v", got)
	}
	f.readyWorkerHost(t, group.ID, pool.ID)
	key := uuid.NewV7().String()
	request := fmt.Sprintf(`{"operation_key":%q}`, key)
	receipt := capacityJSON(t, f.request(t, http.MethodPost, path+"/scale-in", token, request), http.StatusCreated)
	if receipt["operation_key"] != key || receipt["phase"] != "draining" {
		t.Fatalf("receipt: %#v", receipt)
	}
	replay := capacityJSON(t, f.request(t, http.MethodPost, path+"/scale-in", token, request), http.StatusOK)
	read := capacityJSON(t, f.request(t, http.MethodGet, pendingPath+key, token, ""), http.StatusOK)
	for _, got := range []map[string]any{replay, read} {
		if got["worker_host_id"] != receipt["worker_host_id"] || got["drain_claim_version"] != receipt["drain_claim_version"] {
			t.Fatalf("receipt identity changed: %#v", got)
		}
	}
	assertAdminError(t, f.request(t, http.MethodPost, path+"/scale-in", token, fmt.Sprintf(`{"operation_key":%q}`, uuid.NewV7().String())), http.StatusConflict, "scale_in_operation_pending")
}
