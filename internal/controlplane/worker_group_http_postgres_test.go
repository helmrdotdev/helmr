package controlplane

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// workerMember creates a member of orgID holding permissions at organization
// scope and returns its session.
func (f httpPostgresFixture) workerMember(t *testing.T, orgID uuid.UUID, permissions ...string) (uuid.UUID, string) {
	t.Helper()
	user := f.user(t, "Worker member")
	f.member(t, orgID, user, "member")
	if len(permissions) > 0 {
		dbtest.GrantMember(t, f.pool, orgID, user, nil, permissions...)
	}
	return user, f.session(t, user, orgID)
}

// organizationGroupHost gives an organization group a sealed pool with one
// active host and returns the host.
func (f httpPostgresFixture) organizationGroupHost(t *testing.T, group api.WorkerGroup) workergroup.WorkerHost {
	t.Helper()
	groupID := uuid.MustParse(group.ID)
	_, pool, err := workergroup.CreatePool(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), groupID, "run", group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	f.sealWorkerPool(t, group.ID, pgvalue.UUIDString(pool.ID))
	f.readyWorkerHost(t, group.ID, pgvalue.UUIDString(pool.ID))
	hosts, err := workergroup.ListHosts(t.Context(), f.queries, workergroup.HostFilter{OwnerOrgID: uuid.MustParse(organizationOf(t, f, group.ID)), GroupID: groupID, Limit: 10})
	if err != nil || len(hosts.WorkerHosts) != 1 {
		t.Fatalf("hosts = %+v, %v", hosts, err)
	}
	return hosts.WorkerHosts[0]
}

func organizationOf(t *testing.T, f httpPostgresFixture, groupID string) string {
	t.Helper()
	var owner string
	if err := f.pool.QueryRow(t.Context(), `SELECT owner_org_id::text FROM worker_groups WHERE id=$1`, groupID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return owner
}

func lifecycleBody(version int64) string {
	return fmt.Sprintf(`{"expected_claim_version":%d}`, version)
}

func TestOrganizationWorkerGroupHTTPOperatesOwnGroups(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "worker-owner")
	deploymentHTTPEnvironment(t, f, organization)
	_, token := f.workerMember(t, organization, "worker_groups.read", "worker_groups.manage")

	created := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", token,
		`{"region_id":"deployment-test","name":"lab","description":" office "}`), http.StatusCreated)
	group := created.WorkerGroup
	if !strings.HasPrefix(created.EnrollmentToken, "hlmr_wgt_") || group.Status != "active" || group.Description != "office" || organizationOf(t, f, group.ID) != organization.String() {
		t.Fatalf("created = %+v", created)
	}
	listed := decodeAdmin[api.WorkerGroupsResponse](t, f.request(t, http.MethodGet, "/api/worker-groups", token, ""), http.StatusOK)
	if len(listed.WorkerGroups) != 1 || listed.WorkerGroups[0].ID != group.ID {
		t.Fatalf("listed = %+v", listed)
	}
	decodeAdmin[api.WorkerGroup](t, f.request(t, http.MethodGet, "/api/worker-groups/"+group.ID, token, ""), http.StatusOK)

	paused := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/pause", token, lifecycleBody(group.ClaimVersion)), http.StatusOK)
	active := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/activate", token, lifecycleBody(paused.ClaimVersion)), http.StatusOK)
	if paused.Status != "paused" || active.Status != "active" {
		t.Fatalf("paused = %+v, active = %+v", paused, active)
	}
	rotated := decodeAdmin[api.RotateWorkerGroupTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/token/rotate", token, ""), http.StatusOK)
	if rotated.EnrollmentToken == created.EnrollmentToken || !strings.HasPrefix(rotated.EnrollmentToken, "hlmr_wgt_") {
		t.Fatalf("rotated token = %q", rotated.EnrollmentToken)
	}

	group.ClaimVersion = active.ClaimVersion
	host := f.organizationGroupHost(t, group)
	hosts := decodeAdmin[workergroup.ListWorkerHostsResponse](t, f.request(t, http.MethodGet, "/api/worker-groups/"+group.ID+"/hosts", token, ""), http.StatusOK)
	if len(hosts.WorkerHosts) != 1 || hosts.WorkerHosts[0].ID != host.ID || hosts.WorkerHosts[0].Status != workergroup.WorkerHostStatusActive {
		t.Fatalf("hosts = %+v", hosts)
	}
	fenced := decodeAdmin[workergroup.HostStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/hosts/"+host.ID+"/fence", token, lifecycleBody(host.ClaimVersion)), http.StatusOK)
	if fenced.Status != "lost" || !fenced.TransitionApplied {
		t.Fatalf("fenced = %+v", fenced)
	}
	// A replacement enrolled under the same machine resource starts at the same
	// claim version; retrying the first host's fence must not reach it.
	replacement := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts (
        id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,vm_platform_id,
        epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,
        per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,max_vm_starts,
        cpu_environment,cpu_environment_digest,observed_at,epoch_started_at,activated_at,claim_version)
    SELECT $1,resource_id,worker_group_id,worker_pool_id,'active',1,$2,vm_platform_id,
        epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,
        per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,max_vm_starts,
        cpu_environment,cpu_environment_digest,now(),now(),now(),$3
      FROM worker_hosts WHERE id=$4`, replacement, uuid.NewV7(), host.ClaimVersion, host.ID)
	retried := f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/hosts/"+host.ID+"/fence", token, lifecycleBody(host.ClaimVersion))
	var replacementStatus string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM worker_hosts WHERE id=$1`, replacement).Scan(&replacementStatus); err != nil || replacementStatus != "active" {
		t.Fatalf("retried fence (%d %s) reached the replacement: %q %v", retried.Code, retried.Body.String(), replacementStatus, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM worker_hosts WHERE id=$1`, replacement)
	decodeAdmin[api.RevokeWorkerGroupHostCredentialsResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/host-credentials/revoke", token, ""), http.StatusOK)
	current := decodeAdmin[api.WorkerGroup](t, f.request(t, http.MethodGet, "/api/worker-groups/"+group.ID, token, ""), http.StatusOK)
	drained := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/drain", token, lifecycleBody(current.ClaimVersion)), http.StatusOK)
	disabled := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/disable", token, lifecycleBody(drained.ClaimVersion)), http.StatusOK)
	if drained.Status != "draining" || disabled.Status != "disabled" {
		t.Fatalf("drained = %+v, disabled = %+v", drained, disabled)
	}

	// Every operation is recorded in the organization's own audit trail, with
	// the member who performed it.
	var actions []string
	if err := f.pool.QueryRow(t.Context(), `SELECT array_agg(action ORDER BY recorded_at, id) FROM audit_events
 WHERE org_id=$1 AND actor_user_id IS NOT NULL AND (action LIKE 'worker_group.%' OR action LIKE 'worker_host.%')`, organization).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	want := []string{"worker_group.created", "worker_group.paused", "worker_group.activated", "worker_group.token_rotated", "worker_host.marked_lost", "worker_group.host_credentials_revoked", "worker_group.drain_started", "worker_group.disabled"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("audited actions = %v, want %v", actions, want)
	}
}

// Another organization's group, or a deployment group, reads exactly like a
// missing one on every organization route.
func TestOrganizationWorkerGroupHTTPHidesOtherGroups(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	deployment := f.supplyGroup(t, admin, "aws-us-east-1")
	owner, _ := f.organizationOwner(t, "worker-a")
	other, _ := f.organizationOwner(t, "worker-b")
	deploymentHTTPEnvironment(t, f, owner)
	_, ownerToken := f.workerMember(t, owner, "worker_groups.read", "worker_groups.manage")
	_, otherToken := f.workerMember(t, other, "worker_groups.read", "worker_groups.manage")
	group := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", ownerToken,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup
	host := f.organizationGroupHost(t, group)
	missing := uuid.NewV7().String()

	state := func() string {
		var snapshot string
		if err := f.pool.QueryRow(t.Context(), `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(g) ORDER BY g.id) FROM worker_groups g),
 (SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM worker_hosts h),
 (SELECT jsonb_agg(t.token_hash ORDER BY t.id) FROM worker_group_tokens t))::text`).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := state()
	for _, id := range []string{group.ID, deployment.ID, missing} {
		for _, call := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/worker-groups/" + id, ""},
			{http.MethodPost, "/api/worker-groups/" + id + "/pause", lifecycleBody(1)},
			{http.MethodPost, "/api/worker-groups/" + id + "/activate", lifecycleBody(1)},
			{http.MethodPost, "/api/worker-groups/" + id + "/drain", lifecycleBody(1)},
			{http.MethodPost, "/api/worker-groups/" + id + "/disable", lifecycleBody(1)},
			{http.MethodPost, "/api/worker-groups/" + id + "/token/rotate", ""},
			{http.MethodPost, "/api/worker-groups/" + id + "/host-credentials/revoke", ""},
			{http.MethodGet, "/api/worker-groups/" + id + "/hosts", ""},
			{http.MethodPost, "/api/worker-groups/" + id + "/hosts/" + host.ID + "/fence", lifecycleBody(host.ClaimVersion)},
		} {
			token := otherToken
			if id != group.ID {
				token = ownerToken
			}
			response := f.request(t, call.method, call.path, token, call.body)
			if response.Code != http.StatusNotFound || decodeHTTPError(t, response.Body.Bytes()).Code != "not_found" {
				t.Fatalf("%s %s = %d %s, want not_found", call.method, call.path, response.Code, response.Body.String())
			}
		}
	}
	// A host of the caller's group addressed through another group is missing too.
	ownSecond := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", ownerToken,
		`{"region_id":"deployment-test","name":"second"}`), http.StatusCreated).WorkerGroup
	before = state()
	assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+ownSecond.ID+"/hosts/"+host.ID+"/fence", ownerToken, lifecycleBody(host.ClaimVersion)), http.StatusNotFound, "not_found")
	if after := state(); after != before {
		t.Fatalf("refused requests changed state:\nbefore %s\nafter  %s", before, after)
	}
	listed := decodeAdmin[api.WorkerGroupsResponse](t, f.request(t, http.MethodGet, "/api/worker-groups", otherToken, ""), http.StatusOK)
	if len(listed.WorkerGroups) != 0 {
		t.Fatalf("other organization lists %+v", listed)
	}
}

// Each worker group action is authorized on its own permission.
func TestOrganizationWorkerGroupHTTPAuthorizesEachPermission(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "worker-permissions")
	deploymentHTTPEnvironment(t, f, organization)
	_, manager := f.workerMember(t, organization, "worker_groups.manage")
	_, reader := f.workerMember(t, organization, "worker_groups.read")
	_, none := f.workerMember(t, organization)
	group := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup

	for _, token := range []string{manager, none} {
		assertAdminError(t, f.request(t, http.MethodGet, "/api/worker-groups", token, ""), http.StatusForbidden, "forbidden")
		assertAdminError(t, f.request(t, http.MethodGet, "/api/worker-groups/"+group.ID+"/hosts", token, ""), http.StatusForbidden, "forbidden")
	}
	for _, token := range []string{reader, none} {
		assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups", token, `{"region_id":"deployment-test","name":"denied"}`), http.StatusForbidden, "forbidden")
		assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/pause", token, lifecycleBody(group.ClaimVersion)), http.StatusForbidden, "forbidden")
		assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/token/rotate", token, ""), http.StatusForbidden, "forbidden")
	}
	decodeAdmin[api.WorkerGroupsResponse](t, f.request(t, http.MethodGet, "/api/worker-groups", reader, ""), http.StatusOK)
	decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/pause", manager, lifecycleBody(group.ClaimVersion)), http.StatusOK)
}

func TestOrganizationPlacementHTTPRequiresUseForOrganizationGroups(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "placement-owner")
	other, _ := f.organizationOwner(t, "placement-other")
	project, environment := deploymentHTTPEnvironment(t, f, organization)
	deploymentHTTPEnvironment(t, f, other)
	_, manager := f.workerMember(t, organization, "worker_groups.manage")
	_, otherManager := f.workerMember(t, other, "worker_groups.manage")
	group := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup
	foreign := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", otherManager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup

	editorUser, editor := f.workerMember(t, organization)
	dbtest.GrantMember(t, f.pool, organization, editorUser, &environment, "placements.update")
	base := "/api/projects/deployments/environments/staging/placements"
	assertAdminError(t, f.request(t, http.MethodGet, base, editor, ""), http.StatusForbidden, "forbidden")
	decodeAdmin[api.Placement](t, f.request(t, http.MethodPut, base+"/edge", editor, `{"worker_group_id":""}`), http.StatusOK)
	assertAdminError(t, f.request(t, http.MethodPut, base+"/secure", editor, `{"worker_group_id":"`+group.ID+`"}`), http.StatusForbidden, "forbidden")

	dbtest.GrantMember(t, f.pool, organization, editorUser, nil, "worker_groups.use")
	assigned := decodeAdmin[api.Placement](t, f.request(t, http.MethodPut, base+"/secure", editor, `{"worker_group_id":"`+group.ID+`"}`), http.StatusOK)
	if assigned.Name != "secure" || assigned.WorkerGroupID != group.ID {
		t.Fatalf("assigned = %+v", assigned)
	}
	for _, id := range []string{foreign.ID, uuid.NewV7().String()} {
		assertAdminError(t, f.request(t, http.MethodPut, base+"/secure", editor, `{"worker_group_id":"`+id+`"}`), http.StatusBadRequest, "invalid_placement_worker_group")
	}

	readerUser, reader := f.workerMember(t, organization)
	dbtest.GrantMember(t, f.pool, organization, readerUser, &environment, "placements.read")
	listed := decodeAdmin[api.PlacementsResponse](t, f.request(t, http.MethodGet, base, reader, ""), http.StatusOK)
	if fmt.Sprint(listed.Placements) != fmt.Sprint([]api.Placement{{Name: "default"}, {Name: "edge"}, {Name: "secure", WorkerGroupID: group.ID}}) {
		t.Fatalf("placements = %+v", listed.Placements)
	}
	assertAdminError(t, f.request(t, http.MethodPut, base+"/secure", reader, `{"worker_group_id":""}`), http.StatusForbidden, "forbidden")
	if response := f.request(t, http.MethodDelete, base+"/secure", editor, ""); response.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", response.Code, response.Body.String())
	}
	// Environment paths resolve in the caller's organization, so the other
	// organization reaches only its own environment, where this group is
	// foreign.
	otherEditorUser, otherEditor := f.workerMember(t, other, "worker_groups.use")
	var otherEnvironment uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM environments WHERE org_id=$1`, other).Scan(&otherEnvironment); err != nil {
		t.Fatal(err)
	}
	dbtest.GrantMember(t, f.pool, other, otherEditorUser, &otherEnvironment, "placements.update")
	assertAdminError(t, f.request(t, http.MethodPut, base+"/secure", otherEditor, `{"worker_group_id":"`+group.ID+`"}`), http.StatusBadRequest, "invalid_placement_worker_group")

	var details []string
	if err := f.pool.QueryRow(t.Context(), `SELECT array_agg(action || ' ' || details::text ORDER BY recorded_at, id) FROM audit_events
 WHERE environment_id=$1 AND project_id=$2 AND action LIKE 'placement.%'`, environment, project).Scan(&details); err != nil {
		t.Fatal(err)
	}
	want := []string{`placement.assigned {"name": "edge"}`, `placement.assigned {"name": "secure", "worker_group_id": "` + group.ID + `"}`, `placement.removed {"name": "secure"}`}
	if strings.Join(details, "|") != strings.Join(want, "|") {
		t.Fatalf("placement audit = %v, want %v", details, want)
	}
}

// The operator lists organization groups as inventory and may pause one,
// revoke its host credentials and disable it for platform safety, but manages
// nothing else about it.
func TestAdminHTTPTreatsOrganizationGroupsAsInventory(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	f.supplyGroup(t, admin, "aws-us-east-1")
	organization, _ := f.organizationOwner(t, "worker-inventory")
	deploymentHTTPEnvironment(t, f, organization)
	_, token := f.workerMember(t, organization, "worker_groups.manage")
	group := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", token,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup

	listed := decodeAdmin[api.AdminWorkerGroupsResponse](t, f.request(t, http.MethodGet, "/admin/api/v1/worker-groups", admin, ""), http.StatusOK)
	owners := map[string]string{}
	for _, row := range listed.WorkerGroups {
		owners[row.ID] = row.OwnerOrgID
	}
	if owners[group.ID] != organization.String() || len(owners) != 2 {
		t.Fatalf("inventory = %+v", listed)
	}
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPatch, "/admin/api/v1/worker-groups/" + group.ID, `{"description":"operator"}`},
		{http.MethodPost, "/admin/api/v1/worker-groups/" + group.ID + "/activate", lifecycleBody(group.ClaimVersion)},
		{http.MethodPost, "/admin/api/v1/worker-groups/" + group.ID + "/drain", lifecycleBody(group.ClaimVersion)},
		{http.MethodPost, "/admin/api/v1/worker-groups/" + group.ID + "/token/rotate", ""},
		{http.MethodGet, "/admin/api/v1/worker-groups/" + group.ID + "/pools", ""},
		{http.MethodPost, "/admin/api/v1/worker-groups/" + group.ID + "/pools", fmt.Sprintf(`{"name":"run","expected_group_claim_version":%d}`, group.ClaimVersion)},
	} {
		assertAdminError(t, f.request(t, call.method, call.path, admin, call.body), http.StatusNotFound, "not_found")
	}
	host := f.organizationGroupHost(t, group)
	poolID := host.WorkerPoolID
	for _, path := range []string{"/primary", "/drain", "/disable"} {
		assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/pools/"+poolID+path, admin,
			fmt.Sprintf(`{"pool_id":%q,"expected_group_claim_version":1,"expected_pool_claim_version":1,"minimum_ready_hosts":0}`, poolID)), http.StatusNotFound, "not_found")
	}
	paused := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/pause", admin, lifecycleBody(group.ClaimVersion)), http.StatusOK)
	revoked := decodeAdmin[api.RevokeWorkerGroupHostCredentialsResponse](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/host-credentials/revoke", admin, ""), http.StatusOK)
	if paused.Status != "paused" || len(revoked.FencedWorkerHostIDs) != 1 || revoked.FencedWorkerHostIDs[0] != host.ID {
		t.Fatalf("paused = %+v, revoked = %+v", paused, revoked)
	}
	// An active group cannot be disabled before its organization drains it.
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/disable", admin, lifecycleBody(paused.ClaimVersion)), http.StatusConflict, "conflict")
	drained := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/drain", token, lifecycleBody(paused.ClaimVersion)), http.StatusOK)
	disabled := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.ID+"/disable", admin, lifecycleBody(drained.ClaimVersion)), http.StatusOK)
	if disabled.Status != "disabled" {
		t.Fatalf("disabled = %+v", disabled)
	}
	var recorded []string
	if err := f.pool.QueryRow(t.Context(), `SELECT array_agg(action ORDER BY recorded_at, id) FROM audit_events WHERE org_id=$1 AND actor_operator_id IS NOT NULL`, organization).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if strings.Join(recorded, ",") != "worker_group.paused,worker_host.marked_lost,worker_group.host_credentials_revoked,worker_group.disabled" {
		t.Fatalf("operator actions in the organization's audit trail = %v", recorded)
	}
	var pools int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM worker_pools WHERE worker_group_id=$1 AND status<>'disabled'`, group.ID).Scan(&pools); err != nil || pools != 0 {
		t.Fatalf("pools left after disabling the group: %d %v", pools, err)
	}
}
