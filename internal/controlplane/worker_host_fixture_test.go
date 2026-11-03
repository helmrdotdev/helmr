package controlplane

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/golang-jwt/jwt/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testAuthRootKey and testWorkerHostCredentialSigningKey are the keys of
// completeServerConfig.
func testAuthRootKey() []byte { return make([]byte, auth.RootKeySize) }
func testWorkerHostCredentialSigningKey() []byte {
	return make([]byte, workergroup.HostCredentialSigningKeySize)
}

// testHostAuthConfig is the worker host authentication configuration that
// NewServer builds from completeServerConfig, for tests that serve worker
// routes from a Server they assemble themselves.
func testHostAuthConfig(t *testing.T) workergroup.HostAuthConfig {
	t.Helper()
	keys, err := auth.NewKeys(testAuthRootKey())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := workergroup.NewHostAuthConfig(keys.WorkerHost, testWorkerHostCredentialSigningKey(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// newPostgresServer builds the control plane with NewServer from the complete
// test configuration over an existing test database.
func newPostgresServer(t *testing.T, pool *pgxpool.Pool, configure ...func(*ServerConfig)) http.Handler {
	t.Helper()
	queries := db.New(pool)
	cfg := completeServerConfig(t)
	cfg.DB = queries
	cfg.TX = pool
	cfg.DiagnosticDB = pool
	allocator, err := dispatch.NewAllocator(pool, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, sessiontest.ExecutionLimits(), make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Allocator = allocator
	cfg.Auth = identity.NewAPIKeyAuthenticator(pool)
	for _, apply := range configure {
		apply(&cfg)
	}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// Feature fixtures pace ordinary authenticated requests. Worker polling has its
// own lifecycle and must retain its real concurrency. Limit tests use raw handlers.

// seededHostKey identifies a fixture host machine key and its current service.
type seededHostKey struct {
	hostID    uuid.UUID
	keyID     uuid.UUID
	key       ed25519.PrivateKey
	serviceID uuid.UUID
}

// seedHostKey replaces the host's key with a new machine key and sets its
// current service.
func seedHostKey(t *testing.T, pool *pgxpool.Pool, hostID uuid.UUID) seededHostKey {
	t.Helper()
	publicKey, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seeded := seededHostKey{hostID: hostID, keyID: uuid.NewV7(), key: key, serviceID: uuid.NewV7()}
	ctx := t.Context()
	dbtest.MustExec(t, ctx, pool, `UPDATE worker_hosts SET current_service_id=$2 WHERE id=$1`, hostID, seeded.serviceID)
	dbtest.MustExec(t, ctx, pool, `UPDATE worker_host_keys SET revoked_at=now() WHERE worker_host_id=$1 AND revoked_at IS NULL`, hostID)
	dbtest.MustExec(t, ctx, pool, `INSERT INTO worker_host_keys (id,worker_group_id,worker_host_id,public_key,claim_version)
 SELECT $1,worker_group_id,id,$3,claim_version FROM worker_hosts WHERE id=$2`, seeded.keyID, hostID, []byte(publicKey))
	return seeded
}

// client returns a worker client that answers host credential challenges
// with the machine key at baseURL.
func (c seededHostKey) client(t *testing.T, baseURL string) *workerclient.Client {
	t.Helper()
	client, err := workerclient.New(baseURL, workerclient.WithAuth(c.hostID.String(), c.key), workerclient.WithService(c.serviceID.String()), workerclient.WithNetwork(networkpolicy.Installation{}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// issue answers a challenge with the machine key for a host credential
// through handler.
func (c seededHostKey) issue(t *testing.T, handler http.Handler) string {
	t.Helper()
	return issueWorkerHostCredential(t, handler, c.hostID.String(), c.key, c.serviceID.String())
}

// serveWorkerJSON posts body to path through handler.
func serveWorkerJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// requestWorkerHostChallenge asks the served challenge route for a challenge
// for the host and service.
func requestWorkerHostChallenge(t *testing.T, handler http.Handler, hostID string, serviceID string) string {
	t.Helper()
	response := serveWorkerJSON(t, handler, "/worker/v1/instance/challenge", workerapi.HostChallengeRequest{APIVersion: workerapi.APIVersion, WorkerHostID: hostID, ServiceID: serviceID})
	if response.Code != http.StatusOK {
		t.Fatalf("host challenge status = %d: %s", response.Code, response.Body.String())
	}
	var challenge workerapi.HostChallengeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	if challenge.Challenge == "" || challenge.ExpiresInSeconds <= 0 {
		t.Fatalf("host challenge = %+v", challenge)
	}
	return challenge.Challenge
}

// signedHostCredentialRequest requests a challenge through handler and answers
// it with the machine key.
func signedHostCredentialRequest(t *testing.T, handler http.Handler, hostID string, key ed25519.PrivateKey, serviceID string) workerapi.HostCredentialRequest {
	t.Helper()
	challenge := requestWorkerHostChallenge(t, handler, hostID, serviceID)
	return workerapi.HostCredentialRequest{
		APIVersion: workerapi.APIVersion, WorkerHostID: hostID, ServiceID: serviceID,
		Challenge: challenge, Signature: ed25519.Sign(key, auth.HostChallengeMessage(challenge)),
	}
}

// issueWorkerHostCredential answers a host challenge with the machine key for
// a host credential through the served challenge and credential routes.
func issueWorkerHostCredential(t *testing.T, handler http.Handler, hostID string, key ed25519.PrivateKey, serviceID string) string {
	t.Helper()
	response := serveWorkerJSON(t, handler, "/worker/v1/instance/credential", signedHostCredentialRequest(t, handler, hostID, key, serviceID))
	if response.Code != http.StatusOK {
		t.Fatalf("host credential issue status = %d: %s", response.Code, response.Body.String())
	}
	var issued workerapi.HostCredentialResponse
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	return issued.Credential
}

// signRawWorkerJWT signs claims with the completeServerConfig signing key for
// transport tests of host credentials that the credential route never issues.
func signRawWorkerJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["typ"] = "JWT"
	raw, err := token.SignedString(testWorkerHostCredentialSigningKey())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// rawWorkerJWTClaims are well-formed host credential claims for a host, group and
// credential, valid from a minute ago for an hour.
func rawWorkerJWTClaims(hostID string, groupID string, hostKeyID string) jwt.MapClaims {
	now := time.Now().UTC()
	return jwt.MapClaims{
		"iss":                 workergroup.HostCredentialIssuer,
		"sub":                 hostID,
		"aud":                 []string{workergroup.HostCredentialAudience},
		"iat":                 now.Add(-time.Minute).Unix(),
		"exp":                 now.Add(time.Hour).Unix(),
		"worker_group_id":     groupID,
		"worker_host_id":      hostID,
		"host_key_id":         hostKeyID,
		"worker_epoch":        1,
		"claim_version":       1,
		"group_claim_version": 1,
	}
}
