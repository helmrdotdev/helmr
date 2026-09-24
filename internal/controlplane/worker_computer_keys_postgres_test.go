package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestInitialComputerKeyAuthenticatedHTTP(t *testing.T) {
	f, broker, fence := initialKeyFixture(t)
	credentialID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_instance_credentials
 (id,worker_group_id,worker_instance_id,key_prefix,secret_hash,claim_version)
 VALUES ($1,$2,$3,'key-test-prefix',$4,$5)`, credentialID, fence.WorkerGroupID, fence.WorkerID, []byte("test-hash"), fence.ClaimVersion)
	signingKey := bytes.Repeat([]byte{0x49}, 32)
	claims := auth.WorkerClaims{
		WorkerGroupID: pgvalue.UUIDString(fence.WorkerGroupID), WorkerInstanceID: pgvalue.UUIDString(fence.WorkerID),
		CredentialID: credentialID.String(), WorkerEpoch: fence.WorkerEpoch, ClaimVersion: fence.ClaimVersion,
		GroupClaimVersion: fence.GroupClaimVersion, IssuedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}
	issue := func(c auth.WorkerClaims) string {
		t.Helper()
		token, err := auth.IssueWorkerToken(signingKey, c)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	token := issue(claims)
	f.server.computerKeys = broker
	f.server.workerTokenSigningKey = signingKey
	f.server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	router := chi.NewRouter()
	f.server.mountWorkerRoutes(router)
	// Token exchange is independently tested. This fixture supplies a signed token;
	// the production route still authorizes its credential, epoch and claims in SQL.
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/token" {
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: token, ExpiresInSeconds: 3600})
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	client, err := workerclient.New(httpServer.URL, workerclient.WithAuth(claims.WorkerInstanceID, "fixture-secret"), workerclient.WithService(uuid.NewV7().String()))
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.InitialComputerKeyRequest{RuntimeInstanceID: pgvalue.UUIDString(fence.RuntimeID), DesiredVersion: fence.DesiredVersion}
	first, err := client.InitialComputerKey(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Key)
	second, err := client.InitialComputerKey(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(second.Key)
	if first.ID != second.ID || first.Scope != second.Scope || !bytes.Equal(first.Key, second.Key) {
		t.Fatal("lost-response retry changed material")
	}
	payload, _ := json.Marshal(request)
	call := func(bearer string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/worker/v1/run/runtime-instances/initialization/key", bytes.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	if w := call(token, payload); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("successful response status/cache policy: %d", w.Code)
	}
	wrongWorker := claims
	wrongWorker.WorkerInstanceID = uuid.NewV7().String()
	stale := claims
	stale.ClaimVersion++
	expired := claims
	expired.ExpiresAt = time.Now().Add(-time.Second)
	for name, bearer := range map[string]string{"missing": "", "foreign worker": issue(wrongWorker), "stale": issue(stale), "expired": issue(expired)} {
		t.Run(name, func(t *testing.T) {
			w := call(bearer, payload)
			if w.Code != 401 || strings.Contains(w.Body.String(), first.ID) {
				t.Fatalf("unauthorized key response status=%d", w.Code)
			}
		})
	}
	for name, body := range map[string][]byte{
		"caller key": []byte(`{"runtime_instance_id":"` + request.RuntimeInstanceID + `","desired_version":1,"key":"secret"}`),
		"oversized":  []byte(`{"runtime_instance_id":"` + strings.Repeat("x", 1100) + `","desired_version":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			w := call(token, body)
			if w.Code != 400 && w.Code != 413 {
				t.Fatalf("invalid input accepted status=%d", w.Code)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("request body echoed")
			}
		})
	}
	wrongRuntime := request
	wrongRuntime.RuntimeInstanceID = uuid.NewV7().String()
	wrongPayload, _ := json.Marshal(wrongRuntime)
	if w := call(token, wrongPayload); w.Code != 409 {
		t.Fatalf("unowned runtime status=%d", w.Code)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
	if material, err := client.InitialComputerKey(t.Context(), request); err == nil || len(material.Key) != 0 {
		t.Fatal("revoked runtime delivered key")
	}
}

func sourceKeyHTTPClient(t *testing.T, f initialPublicationFixture, broker *computerKeyBroker, fence computerKeyFence) *workerclient.Client {
	t.Helper()
	credentialID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_instance_credentials
 (id,worker_group_id,worker_instance_id,key_prefix,secret_hash,claim_version)
 VALUES ($1,$2,$3,'key-test-prefix',$4,$5)`, credentialID, fence.WorkerGroupID, fence.WorkerID, []byte("test-hash"), fence.ClaimVersion)
	signingKey := bytes.Repeat([]byte{0x49}, 32)
	claims := auth.WorkerClaims{
		WorkerGroupID: pgvalue.UUIDString(fence.WorkerGroupID), WorkerInstanceID: pgvalue.UUIDString(fence.WorkerID),
		CredentialID: credentialID.String(), WorkerEpoch: fence.WorkerEpoch, ClaimVersion: fence.ClaimVersion,
		GroupClaimVersion: fence.GroupClaimVersion, IssuedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}
	issue := func(c auth.WorkerClaims) string {
		t.Helper()
		token, err := auth.IssueWorkerToken(signingKey, c)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	token := issue(claims)
	f.server.computerKeys = broker
	f.server.workerTokenSigningKey = signingKey
	f.server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	router := chi.NewRouter()
	f.server.mountWorkerRoutes(router)
	// Token exchange is independently tested. This fixture supplies a signed token;
	// the production route still authorizes its credential, epoch and claims in SQL.
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/token" {
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: token, ExpiresInSeconds: 3600})
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	client, err := workerclient.New(httpServer.URL, workerclient.WithAuth(claims.WorkerInstanceID, "fixture-secret"), workerclient.WithService(uuid.NewV7().String()))
	if err != nil {
		t.Fatal(err)
	}

	return client
}
