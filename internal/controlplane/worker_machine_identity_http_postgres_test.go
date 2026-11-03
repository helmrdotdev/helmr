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

	expired := decodeAdmin[api.IssueEnrollmentTokenResponse](t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", manager, ""), http.StatusCreated)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_enrollment_tokens SET created_at=created_at-interval '2 hours', expires_at=clock_timestamp()-interval '1 second' WHERE consumed_at IS NULL`)
	assertAdminError(t, f.machineEnrollment(t, expired.EnrollmentToken, "desktop", second), http.StatusUnauthorized, "unauthorized")

	var consumedBy uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT worker_host_id FROM worker_enrollment_tokens WHERE consumed_at IS NOT NULL`).Scan(&consumedBy); err != nil || consumedBy.String() != enrolled.WorkerHostID {
		t.Fatalf("consumed by %s, %v; want %s", consumedBy, err, enrolled.WorkerHostID)
	}
	var audited int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='worker_group.enrollment_token_issued' AND org_id=$1 AND target_id=$2`, organization, group.ID).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("issuance audit count = %d, %v", audited, err)
	}
	// Another organization cannot issue tokens for this group.
	other, _ := f.organizationOwner(t, "single-use-other")
	_, otherManager := f.workerMember(t, other, "worker_groups.manage")
	assertAdminError(t, f.request(t, http.MethodPost, "/api/worker-groups/"+group.ID+"/enrollment-tokens", otherManager, ""), http.StatusNotFound, "not_found")
}
