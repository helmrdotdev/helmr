package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// An organization's machine fenced by its operator returns on its own by
// proving its key. Revoking the key stops that, and declaring the machine
// permanently lost releases its custody. Both need worker_groups.manage in the
// owning organization.
func TestOrganizationHostReturnsUntilRevoked(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "host-return")
	deploymentHTTPEnvironment(t, f, organization)
	_, manager := f.workerMember(t, organization, "worker_groups.read", "worker_groups.manage")
	_, reader := f.workerMember(t, organization, "worker_groups.read")
	other, _ := f.organizationOwner(t, "host-return-other")
	_, otherManager := f.workerMember(t, other, "worker_groups.manage")
	created := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated)
	group := created.WorkerGroup
	public, private := newMachineKey(t)
	var enrolled workerapi.EnrollmentResponse
	if err := json.Unmarshal(f.machineEnrollment(t, created.EnrollmentToken, "laptop", public).Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	service := uuid.NewV7().String()
	code, first := f.machineCredential(t, enrolled.WorkerHostID, service, private, nil)
	if code != http.StatusOK {
		t.Fatalf("first credential = %d", code)
	}
	host := func() workergroup.WorkerHost {
		t.Helper()
		hosts := decodeAdmin[workergroup.ListWorkerHostsResponse](t, f.request(t, http.MethodGet, "/api/worker-groups/"+group.ID+"/hosts", manager, ""), http.StatusOK)
		if len(hosts.WorkerHosts) != 1 {
			t.Fatalf("hosts = %+v", hosts)
		}
		return hosts.WorkerHosts[0]
	}
	hostPath := "/api/worker-groups/" + group.ID + "/hosts/" + enrolled.WorkerHostID
	decodeAdmin[workergroup.HostStatus](t, f.request(t, http.MethodPost, hostPath+"/fence", manager, lifecycleBody(host().ClaimVersion)), http.StatusOK)
	if got := host(); got.Status != workergroup.WorkerHostStatusLost {
		t.Fatalf("fenced host = %+v", got)
	}

	// The machine proves its key again, with no operator step.
	code, returned := f.machineCredential(t, enrolled.WorkerHostID, service, private, nil)
	if code != http.StatusOK || returned.WorkerEpoch <= first.WorkerEpoch {
		t.Fatalf("returned credential = %d %+v, want an epoch past %d", code, returned, first.WorkerEpoch)
	}
	if got := host(); got.Status != workergroup.WorkerHostStatusRegistering {
		t.Fatalf("returned host = %+v", got)
	}

	for _, action := range []string{"/revoke", "/permanently-lost"} {
		assertAdminError(t, f.request(t, http.MethodPost, hostPath+action, reader, ""), http.StatusForbidden, "forbidden")
		assertAdminError(t, f.request(t, http.MethodPost, hostPath+action, otherManager, ""), http.StatusNotFound, "not_found")
	}
	revoked := decodeAdmin[workergroup.HostStatus](t, f.request(t, http.MethodPost, hostPath+"/revoke", manager, ""), http.StatusOK)
	if revoked.Status != "lost" || !revoked.TransitionApplied {
		t.Fatalf("revoked = %+v", revoked)
	}
	if code, _ := f.machineCredential(t, enrolled.WorkerHostID, service, private, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked key credential = %d", code)
	}
	declared := decodeAdmin[workergroup.WorkerHost](t, f.request(t, http.MethodPost, hostPath+"/permanently-lost", manager, ""), http.StatusOK)
	if declared.Status != workergroup.WorkerHostStatusLost {
		t.Fatalf("declared = %+v", declared)
	}
	var actions []string
	if err := f.pool.QueryRow(t.Context(), `SELECT array_agg(action ORDER BY recorded_at, id) FROM audit_events WHERE org_id=$1 AND target_id=$2`, organization, enrolled.WorkerHostID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	want := []string{"worker_host.marked_lost", "worker_host.returned", "worker_host.marked_lost", "worker_host.key_revoked", "worker_host.declared_permanently_lost"}
	if len(actions) != len(want) {
		t.Fatalf("audited = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("audited = %v, want %v", actions, want)
		}
	}
}

// A host still reporting cannot be declared permanently lost: the attestation
// would release custody of a machine that may still run its VMs.
func TestPermanentLossRequiresALostHost(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "live-loss")
	deploymentHTTPEnvironment(t, f, organization)
	_, manager := f.workerMember(t, organization, "worker_groups.manage")
	created := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated)
	public, _ := newMachineKey(t)
	var enrolled workerapi.EnrollmentResponse
	if err := json.Unmarshal(f.machineEnrollment(t, created.EnrollmentToken, "laptop", public).Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+created.WorkerGroup.ID+"/hosts/"+enrolled.WorkerHostID+"/permanently-lost", manager, ""), http.StatusConflict, "conflict")
}
