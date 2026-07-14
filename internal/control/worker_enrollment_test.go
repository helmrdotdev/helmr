package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerEnrollmentStore struct {
	*fakeStore
	challenge  db.CreateWorkerEnrollmentNonceParams
	enrollment db.EnrollWorkerInstanceParams
}

func (s *workerEnrollmentStore) CreateWorkerEnrollmentNonce(_ context.Context, arg db.CreateWorkerEnrollmentNonceParams) (db.WorkerEnrollmentNonce, error) {
	s.challenge = arg
	return db.WorkerEnrollmentNonce{
		ID: arg.ID, NonceHash: arg.NonceHash, WorkerGroupID: arg.WorkerGroupID,
		ExpiresAt: arg.ExpiresAt, CreatedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}, nil
}

func (s *workerEnrollmentStore) EnrollWorkerInstance(_ context.Context, arg db.EnrollWorkerInstanceParams) (db.EnrollWorkerInstanceRow, error) {
	s.enrollment = arg
	return db.EnrollWorkerInstanceRow{
		ID: arg.CredentialID, WorkerGroupID: arg.WorkerGroupID, WorkerInstanceID: arg.WorkerInstanceID,
		KeyPrefix: arg.KeyPrefix, AllowsRun: arg.AllowsRun, AllowsBuild: arg.AllowsBuild,
		ProtocolVersion: arg.ProtocolVersion, SecretHash: arg.SecretHash,
	}, nil
}

func (s *workerEnrollmentStore) GetActiveWorkerEnrollmentNonce(_ context.Context, arg db.GetActiveWorkerEnrollmentNonceParams) (db.WorkerEnrollmentNonce, error) {
	if arg.WorkerGroupID != s.challenge.WorkerGroupID || string(arg.NonceHash) != string(s.challenge.NonceHash) {
		return db.WorkerEnrollmentNonce{}, pgx.ErrNoRows
	}
	return db.WorkerEnrollmentNonce{
		ID: s.challenge.ID, NonceHash: s.challenge.NonceHash,
		WorkerGroupID: s.challenge.WorkerGroupID, ExpiresAt: s.challenge.ExpiresAt,
	}, nil
}

type fixedWorkerEnrollmentVerifier struct {
	verified VerifiedWorkerEnrollment
	err      error
}

func (v fixedWorkerEnrollmentVerifier) VerifyWorkerEnrollment(context.Context, api.WorkerEnrollmentRequest) (VerifiedWorkerEnrollment, error) {
	return v.verified, v.err
}

func TestWorkerEnrollmentBindsVerifiedIdentityToOneTimeChallenge(t *testing.T) {
	store := &workerEnrollmentStore{fakeStore: &fakeStore{}}
	verifier := fixedWorkerEnrollmentVerifier{verified: VerifiedWorkerEnrollment{
		WorkerGroupID: "run-workers", ResourceID: "i-0123456789abcdef0",
		AllowsRun: true, ProtocolVersion: auth.WorkerProtocolVersion,
		EnrollmentPolicyFingerprint: "sha256:verified-policy",
		AttestationFingerprint:      "sha256:verified-attestation",
	}}
	handler := newTestServer(testServerConfig{
		DeploymentMode: deploymentModeManagedCloud,
		DB:             store, AuthSecret: []byte("01234567890123456789012345678901"), WorkerEnrollment: verifier,
	})

	challengeRequest := httptest.NewRequest(http.MethodPost, "/api/worker/enrollment/challenge", strings.NewReader(`{"worker_group_id":"run-workers"}`))
	challengeRequest.Header.Set("content-type", "application/json")
	challengeResponse := httptest.NewRecorder()
	handler.ServeHTTP(challengeResponse, challengeRequest)
	if challengeResponse.Code != http.StatusCreated {
		t.Fatalf("challenge status = %d, body = %s", challengeResponse.Code, challengeResponse.Body.String())
	}
	var challenge api.WorkerEnrollmentChallengeResponse
	if err := json.NewDecoder(challengeResponse.Body).Decode(&challenge); err != nil {
		t.Fatal(err)
	}
	if challenge.Nonce == "" || challenge.WorkerGroupID != "run-workers" || challenge.ProtocolVersion != auth.WorkerProtocolVersion {
		t.Fatalf("challenge = %+v", challenge)
	}
	if len(store.challenge.NonceHash) == 0 || store.challenge.WorkerGroupID != "run-workers" {
		t.Fatalf("stored challenge = %+v", store.challenge)
	}

	body, err := json.Marshal(api.WorkerEnrollmentRequest{
		WorkerGroupID: "run-workers", Nonce: challenge.Nonce,
		SupportsRun: true, ProtocolVersion: auth.WorkerProtocolVersion,
		InstanceIdentityDocument: json.RawMessage(`{"instanceId":"i-0123456789abcdef0"}`),
		SignedSTSRequest:         api.SignedHTTPRequest{Method: http.MethodPost, URL: "https://sts.us-east-1.amazonaws.com/", Headers: map[string][]string{"authorization": {"signed"}}, Body: "Action=GetCallerIdentity&Version=2011-06-15"},
	})
	if err != nil {
		t.Fatal(err)
	}
	enrollRequest := httptest.NewRequest(http.MethodPost, "/api/worker/enrollment", strings.NewReader(string(body)))
	enrollRequest.Header.Set("content-type", "application/json")
	enrollResponse := httptest.NewRecorder()
	handler.ServeHTTP(enrollResponse, enrollRequest)
	if enrollResponse.Code != http.StatusCreated {
		t.Fatalf("enroll status = %d, body = %s", enrollResponse.Code, enrollResponse.Body.String())
	}
	if store.enrollment.WorkerGroupID != "run-workers" || store.enrollment.ResourceID != "i-0123456789abcdef0" || !store.enrollment.AllowsRun || store.enrollment.AllowsBuild || store.enrollment.ProtocolVersion != auth.WorkerProtocolVersion {
		t.Fatalf("enrollment = %+v", store.enrollment)
	}
	if len(store.enrollment.NonceHash) == 0 || string(store.enrollment.NonceHash) != string(store.challenge.NonceHash) {
		t.Fatal("enrollment did not bind the issued challenge")
	}
}

func TestWorkerEnrollmentRejectsUnverifiedEvidence(t *testing.T) {
	authSecret := []byte("01234567890123456789012345678901")
	nonceHash, err := auth.HashToken(authSecret, "nonce")
	if err != nil {
		t.Fatal(err)
	}
	store := &workerEnrollmentStore{fakeStore: &fakeStore{}, challenge: db.CreateWorkerEnrollmentNonceParams{
		NonceHash: nonceHash, WorkerGroupID: "run-workers",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}}
	handler := newTestServer(testServerConfig{
		DeploymentMode:   deploymentModeManagedCloud,
		DB:               store,
		AuthSecret:       authSecret,
		WorkerEnrollment: fixedWorkerEnrollmentVerifier{err: errors.New("invalid signature")},
	})
	body := `{"worker_group_id":"run-workers","nonce":"nonce","instance_identity_document":{"instanceId":"i-1"},"signed_sts_request":{"method":"POST","url":"https://sts.us-east-1.amazonaws.com/","headers":{"authorization":["signed"]},"body":"Action=GetCallerIdentity"},"supports_run":true,"protocol_version":"helmr.worker.v0"}`
	request := httptest.NewRequest(http.MethodPost, "/api/worker/enrollment", strings.NewReader(body))
	request.Header.Set("content-type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestWorkerEnrollmentRejectsLegacyProvisioningFields(t *testing.T) {
	handler := newTestServer(testServerConfig{
		DB:               &workerEnrollmentStore{fakeStore: &fakeStore{}},
		AuthSecret:       []byte("01234567890123456789012345678901"),
		WorkerEnrollment: fixedWorkerEnrollmentVerifier{},
	})
	body := `{"worker_group_id":"run-workers","nonce":"nonce","provisioning_token":"operator-token","resource_id":"host-01","supports_run":true,"protocol_version":"helmr.worker.v0"}`
	request := httptest.NewRequest(http.MethodPost, "/api/worker/enrollment", strings.NewReader(body))
	request.Header.Set("content-type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}
