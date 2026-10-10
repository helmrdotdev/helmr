package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/golang-jwt/jwt/v5"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
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
	allocator, err := agent.NewAllocator(pool, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Allocator = allocator
	cfg.Auth = identity.NewAPIKeyAuthenticator(queries)
	for _, apply := range configure {
		apply(&cfg)
	}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// seededHostSecret is a host secret for a worker host that a test seeded
// directly in the database, bound to the service that holds its current
// epoch.
type seededHostSecret struct {
	hostID    uuid.UUID
	secret    string
	serviceID uuid.UUID
}

// seedHostSecret gives a seeded worker host a host secret keyed like
// completeServerConfig, revoking its other host secrets, and binds the host's
// current epoch to a service so that exchanging the secret keeps that epoch.
// The host secret's key prefix is the whole secret, which keeps it unique.
func seedHostSecret(t *testing.T, pool *pgxpool.Pool, hostID uuid.UUID) seededHostSecret {
	t.Helper()
	keys, err := auth.NewKeys(testAuthRootKey())
	if err != nil {
		t.Fatal(err)
	}
	seeded := seededHostSecret{hostID: hostID, secret: "hlmr_wi_" + uuid.NewV7().String(), serviceID: uuid.NewV7()}
	hash, err := auth.HashToken(keys.WorkerHost, seeded.secret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	dbtest.MustExec(t, ctx, pool, `UPDATE worker_hosts SET current_service_id=$2 WHERE id=$1`, hostID, seeded.serviceID)
	dbtest.MustExec(t, ctx, pool, `UPDATE worker_host_secrets SET revoked_at=now() WHERE worker_host_id=$1 AND revoked_at IS NULL`, hostID)
	dbtest.MustExec(t, ctx, pool, `INSERT INTO worker_host_secrets (id,worker_group_id,worker_host_id,key_prefix,secret_hash,claim_version)
 SELECT $1,worker_group_id,id,$4,$3,claim_version FROM worker_hosts WHERE id=$2`, uuid.NewV7(), hostID, hash, seeded.secret)
	return seeded
}

// client returns a worker client that exchanges the secret for host
// credentials at baseURL.
func (c seededHostSecret) client(t *testing.T, baseURL string) *workerclient.Client {
	t.Helper()
	client, err := workerclient.New(baseURL, workerclient.WithAuth(c.hostID.String(), c.secret), workerclient.WithService(c.serviceID.String()))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// issue exchanges the secret for a host credential through handler.
func (c seededHostSecret) issue(t *testing.T, handler http.Handler) string {
	t.Helper()
	return issueWorkerHostCredential(t, handler, c.hostID.String(), c.secret, c.serviceID.String())
}

// issueWorkerHostCredential exchanges a host secret for a host credential
// through the served credential route.
func issueWorkerHostCredential(t *testing.T, handler http.Handler, hostID string, secret string, serviceID string) string {
	t.Helper()
	body, err := json.Marshal(workerapi.HostCredentialRequest{APIVersion: workerapi.APIVersion, WorkerHostID: hostID, WorkerHostSecret: secret, ServiceID: serviceID})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/instance/credential", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
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
func rawWorkerJWTClaims(hostID string, groupID string, hostSecretID string) jwt.MapClaims {
	now := time.Now().UTC()
	return jwt.MapClaims{
		"iss":                 workergroup.HostCredentialIssuer,
		"sub":                 hostID,
		"aud":                 []string{workergroup.HostCredentialAudience},
		"iat":                 now.Add(-time.Minute).Unix(),
		"exp":                 now.Add(time.Hour).Unix(),
		"worker_group_id":     groupID,
		"worker_host_id":      hostID,
		"host_secret_id":      hostSecretID,
		"worker_epoch":        1,
		"claim_version":       1,
		"group_claim_version": 1,
	}
}
