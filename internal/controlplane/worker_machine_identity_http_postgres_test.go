package controlplane

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// machineEnrollment enrolls a machine's public key with enrollmentToken.
func (f httpPostgresFixture) machineEnrollment(t *testing.T, enrollmentToken, resourceID string, public ed25519.PublicKey) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(workerapi.EnrollmentRequest{APIVersion: workerapi.APIVersion, ResourceID: resourceID, PoolName: "run", PublicKey: public})
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, http.MethodPost, "/worker/v1/enrollment", enrollmentToken, string(body))
}

// machineCredential answers a fresh challenge for the host with key, signing
// sign(challenge) instead when it is set.
func (f httpPostgresFixture) machineCredential(t *testing.T, hostID, serviceID string, key ed25519.PrivateKey, sign func(challenge string) []byte) (int, workerapi.HostCredentialResponse) {
	t.Helper()
	request, err := json.Marshal(workerapi.HostChallengeRequest{APIVersion: workerapi.APIVersion, WorkerHostID: hostID, ServiceID: serviceID})
	if err != nil {
		t.Fatal(err)
	}
	challenge := decodeAdmin[workerapi.HostChallengeResponse](t, f.request(t, http.MethodPost, "/worker/v1/instance/challenge", "", string(request)), http.StatusOK)
	if challenge.Challenge == "" || challenge.ExpiresInSeconds <= 0 {
		t.Fatalf("challenge = %+v", challenge)
	}
	signature := ed25519.Sign(key, auth.HostChallengeMessage(challenge.Challenge))
	if sign != nil {
		signature = sign(challenge.Challenge)
	}
	answer, err := json.Marshal(workerapi.HostCredentialRequest{APIVersion: workerapi.APIVersion, WorkerHostID: hostID, ServiceID: serviceID, Challenge: challenge.Challenge, Signature: signature})
	if err != nil {
		t.Fatal(err)
	}
	response := f.request(t, http.MethodPost, "/worker/v1/instance/credential", "", string(answer))
	var credential workerapi.HostCredentialResponse
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &credential); err != nil {
			t.Fatal(err)
		}
	}
	return response.Code, credential
}

func newMachineKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

// A machine proves itself only with the key it enrolled: no secret is ever
// returned, a foreign signature or a challenge for another host or service is
// refused, and a revoked key can neither authenticate nor enroll again.
func TestWorkerMachineKeyAuthenticatesOnlyItsHost(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	group := f.createSupplyGroup(t, admin, "us-east")
	public, private := newMachineKey(t)
	response := f.machineEnrollment(t, group.EnrollmentToken, "machine-a", public)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("enrollment = %d %s", response.Code, response.Body.String())
	}
	var enrolled workerapi.EnrollmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	service := uuid.NewV7().String()
	if code, credential := f.machineCredential(t, enrolled.WorkerHostID, service, private, nil); code != http.StatusOK || credential.Credential == "" || credential.WorkerEpoch <= 0 {
		t.Fatalf("credential = %d %+v", code, credential)
	}
	_, other := newMachineKey(t)
	if code, _ := f.machineCredential(t, enrolled.WorkerHostID, service, other, nil); code != http.StatusUnauthorized {
		t.Fatalf("foreign key credential = %d", code)
	}
	// A challenge answered for a different service, or presented for a
	// different host, is refused.
	otherService := uuid.NewV7().String()
	if code, _ := f.machineCredential(t, enrolled.WorkerHostID, service, private, func(string) []byte {
		request, _ := json.Marshal(workerapi.HostChallengeRequest{APIVersion: workerapi.APIVersion, WorkerHostID: enrolled.WorkerHostID, ServiceID: otherService})
		other := decodeAdmin[workerapi.HostChallengeResponse](t, f.request(t, http.MethodPost, "/worker/v1/instance/challenge", "", string(request)), http.StatusOK)
		return ed25519.Sign(private, auth.HostChallengeMessage(other.Challenge))
	}); code != http.StatusUnauthorized {
		t.Fatalf("signature over another challenge = %d", code)
	}
	if code, _ := f.machineCredential(t, uuid.NewV7().String(), service, private, nil); code != http.StatusUnauthorized {
		t.Fatalf("credential for a missing host = %d", code)
	}

	decodeAdmin[api.RevokeWorkerGroupHostCredentialsResponse](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+group.WorkerGroup.ID+"/host-credentials/revoke", admin, ""), http.StatusOK)
	if code, _ := f.machineCredential(t, enrolled.WorkerHostID, service, private, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked key credential = %d", code)
	}
	again := f.machineEnrollment(t, group.EnrollmentToken, "machine-a", public)
	assertAdminError(t, again, http.StatusConflict, "worker_key_registered")
	fresh, _ := newMachineKey(t)
	if response := f.machineEnrollment(t, group.EnrollmentToken, "machine-a", fresh); response.Code != http.StatusCreated {
		t.Fatalf("enrollment with a new key = %d %s", response.Code, response.Body.String())
	}
}

// A single-use enrollment token admits exactly one machine to its group
// before it expires.
func TestSingleUseEnrollmentTokenAdmitsOneMachine(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "single-use")
	deploymentHTTPEnvironment(t, f, organization)
	_, manager := f.workerMember(t, organization, "worker_groups.manage")
	_, reader := f.workerMember(t, organization, "worker_groups.read")
	group := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup
	assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", reader, ""), http.StatusForbidden, "forbidden")

	issued := decodeAdmin[api.IssueEnrollmentTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", manager, ""), http.StatusCreated)
	if !strings.HasPrefix(issued.EnrollmentToken, "hlmr_wet_") || issued.ExpiresAt.Before(time.Now().Add(50*time.Minute)) || issued.ExpiresAt.After(time.Now().Add(70*time.Minute)) {
		t.Fatalf("issued = %+v", issued)
	}
	first, _ := newMachineKey(t)
	response := f.machineEnrollment(t, issued.EnrollmentToken, "laptop", first)
	if response.Code != http.StatusCreated {
		t.Fatalf("first enrollment = %d %s", response.Code, response.Body.String())
	}
	var enrolled workerapi.EnrollmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &enrolled); err != nil || enrolled.WorkerGroupID != group.ID {
		t.Fatalf("enrolled = %+v, %v", enrolled, err)
	}
	second, _ := newMachineKey(t)
	assertAdminError(t, f.machineEnrollment(t, issued.EnrollmentToken, "desktop", second), http.StatusUnauthorized, "unauthorized")

	// A single-use token cannot re-register, and so re-key, a host already
	// enrolled under that resource ID; the token stays unused.
	takeover := decodeAdmin[api.IssueEnrollmentTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", manager, ""), http.StatusCreated)
	assertAdminError(t, f.machineEnrollment(t, takeover.EnrollmentToken, "laptop", second), http.StatusConflict, "worker_resource_enrolled")
	var keys int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM worker_host_keys WHERE worker_host_id=$1 AND revoked_at IS NULL AND public_key=$2`, enrolled.WorkerHostID, []byte(first)).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("enrolled host key replaced: %d %v", keys, err)
	}
	var unused bool
	if err := f.pool.QueryRow(t.Context(), `SELECT consumed_at IS NULL FROM worker_enrollment_tokens WHERE token_hash=$1`, auth.HashCredential(takeover.EnrollmentToken)).Scan(&unused); err != nil || !unused {
		t.Fatalf("refused enrollment consumed its token: %t %v", unused, err)
	}
	// Rotating the group's token also stops the unused single-use token.
	decodeAdmin[api.RotateWorkerGroupTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/token/rotate", manager, ""), http.StatusOK)
	assertAdminError(t, f.machineEnrollment(t, takeover.EnrollmentToken, "desktop", second), http.StatusUnauthorized, "unauthorized")

	expired := decodeAdmin[api.IssueEnrollmentTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", manager, ""), http.StatusCreated)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_enrollment_tokens SET created_at=created_at-interval '2 hours', expires_at=clock_timestamp()-interval '1 second' WHERE consumed_at IS NULL AND token_hash=$1`, auth.HashCredential(expired.EnrollmentToken))
	assertAdminError(t, f.machineEnrollment(t, expired.EnrollmentToken, "desktop", second), http.StatusUnauthorized, "unauthorized")

	var consumedBy uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT worker_host_id FROM worker_enrollment_tokens WHERE consumed_at IS NOT NULL`).Scan(&consumedBy); err != nil || consumedBy.String() != enrolled.WorkerHostID {
		t.Fatalf("consumed by %s, %v; want %s", consumedBy, err, enrolled.WorkerHostID)
	}
	var audited int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='worker_group.enrollment_token_issued' AND org_id=$1 AND target_id=$2`, organization, group.ID).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("issuance audit count = %d, %v", audited, err)
	}
	// Another organization cannot issue tokens for this group.
	other, _ := f.organizationOwner(t, "single-use-other")
	_, otherManager := f.workerMember(t, other, "worker_groups.manage")
	assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", otherManager, ""), http.StatusNotFound, "not_found")
}

// Repeating an enrollment whose response was lost returns the same host for
// the same token, resource and key, and nothing else.
func TestEnrollmentReplayReturnsTheSameHost(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "replay")
	deploymentHTTPEnvironment(t, f, organization)
	_, manager := f.workerMember(t, organization, "worker_groups.manage")
	created := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated)
	enroll := func(token, resource string, key ed25519.PublicKey) workerapi.EnrollmentResponse {
		t.Helper()
		return decodeAdmin[workerapi.EnrollmentResponse](t, f.machineEnrollment(t, token, resource, key), http.StatusCreated)
	}
	issued := decodeAdmin[api.IssueEnrollmentTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+created.WorkerGroup.ID+"/enrollment-tokens", manager, ""), http.StatusCreated)
	key, _ := newMachineKey(t)
	first := enroll(issued.EnrollmentToken, "laptop", key)
	if replay := enroll(issued.EnrollmentToken, "laptop", key); replay != first {
		t.Fatalf("single-use replay = %+v, want %+v", replay, first)
	}
	other, _ := newMachineKey(t)
	assertAdminError(t, f.machineEnrollment(t, issued.EnrollmentToken, "laptop", other), http.StatusUnauthorized, "unauthorized")
	assertAdminError(t, f.machineEnrollment(t, issued.EnrollmentToken, "desktop", key), http.StatusUnauthorized, "unauthorized")

	fleet, _ := newMachineKey(t)
	host := enroll(created.EnrollmentToken, "instance", fleet)
	if replay := enroll(created.EnrollmentToken, "instance", fleet); replay != host {
		t.Fatalf("group token replay = %+v, want %+v", replay, host)
	}
	var hosts, keys int
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM worker_hosts WHERE worker_group_id=$1), (SELECT count(*) FROM worker_host_keys WHERE worker_group_id=$1)`, created.WorkerGroup.ID).Scan(&hosts, &keys); err != nil || hosts != 2 || keys != 2 {
		t.Fatalf("hosts = %d, keys = %d, %v; want one of each per machine", hosts, keys, err)
	}
}

// A single-use token that expires while its enrollment waits for the group
// lock is not consumed.
func TestSingleUseEnrollmentTokenExpiresDuringLockWait(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "expiry-wait")
	deploymentHTTPEnvironment(t, f, organization)
	_, manager := f.workerMember(t, organization, "worker_groups.manage")
	group := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup
	issued := decodeAdmin[api.IssueEnrollmentTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", manager, ""), http.StatusCreated)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_enrollment_tokens SET expires_at=clock_timestamp()+interval '1 second'`)
	hold, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(t.Context())
	if _, err := hold.Exec(t.Context(), `SELECT id FROM worker_groups WHERE id=$1 FOR UPDATE`, group.ID); err != nil {
		t.Fatal(err)
	}
	key, _ := newMachineKey(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.machineEnrollment(t, issued.EnrollmentToken, "laptop", key) }()
	time.Sleep(1500 * time.Millisecond)
	if err := hold.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertAdminError(t, <-done, http.StatusUnauthorized, "unauthorized")
	var consumed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT consumed_at IS NOT NULL OR EXISTS(SELECT 1 FROM worker_hosts WHERE worker_group_id=$1) FROM worker_enrollment_tokens`, group.ID).Scan(&consumed); err != nil || consumed {
		t.Fatalf("expired token consumed or host created: %t %v", consumed, err)
	}
}

// Issuing a single-use token needs recent sign-in proof and a group that can
// still admit hosts.
func TestSingleUseEnrollmentTokenIssuanceGuards(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	organization, _ := f.organizationOwner(t, "issuance-guards")
	deploymentHTTPEnvironment(t, f, organization)
	user, manager := f.workerMember(t, organization, "worker_groups.manage")
	group := decodeAdmin[api.CreateWorkerGroupResponse](t, f.request(t, http.MethodPost, "/api/worker-groups", manager,
		`{"region_id":"deployment-test","name":"lab"}`), http.StatusCreated).WorkerGroup
	drained := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/drain", manager, lifecycleBody(group.ClaimVersion)), http.StatusOK)
	if drained.Status != "draining" {
		t.Fatalf("drained = %+v", drained)
	}
	assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", manager, ""), http.StatusConflict, "conflict")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET authenticated_at=authenticated_at-interval '1 day' WHERE user_id=$1`, user)
	if response := f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", manager, ""); response.Code != http.StatusForbidden {
		t.Fatalf("stale session issued a token: %d %s", response.Code, response.Body.String())
	}
	var tokens int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM worker_enrollment_tokens`).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("tokens = %d, %v", tokens, err)
	}
}
