package controlplane

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// This fixture inserts already allocated trust to isolate authenticated transport.
// Agent allocation tests independently exercise transactional CA creation/rollback.
func TestWorkerPreparationProtectedSecretsUseHostProxyAndPinnedVersion(t *testing.T) {
	f, executor := preparationTransportFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=clock_timestamp() WHERE id=$1`, f.Worker)
	store, err := secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(t.Context(), f.Environment, "BUILD_TOKEN", []byte("protected-first"), "create-protected")
	if err != nil {
		t.Fatal(err)
	}
	secretID := pgvalue.MustUUIDValue(record.ID)
	marker, err := secretbinding.Placeholder("protected")
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,preparation_spec_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','TOKEN','protected',ARRAY['https://api.example.com'],$4)`, f.Environment, f.Deployment, secretID, marker)
	var now, deadline time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT clock_timestamp(),deadline_at FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.Environment, executor.Identity.OwnerID).Scan(&now, &deadline); err != nil {
		t.Fatal(err)
	}
	trust, err := store.GeneratePreparationProxyTrust(f.Environment, uuid.MustParse(executor.Identity.OwnerID), now, deadline)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET proxy_ca_certificate=$3,proxy_ca_private_key_nonce=$4,proxy_ca_private_key_ciphertext=$5,proxy_ca_not_after=$6 WHERE environment_id=$1 AND id=$2`, f.Environment, executor.Identity.OwnerID, trust.Certificate, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext, trust.NotAfter)
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.SecretProxy = store; cfg.SecretDelivery = store }))
	defer server.Close()
	auth := seedHostSecret(t, f.Pool, f.Worker)
	client := auth.client(t, server.URL)
	request := workerapi.SecretProxyRequest{ComputerInstanceID: executor.Identity.InstanceID}
	leaf, err := client.PrepareSecretProxy(t.Context(), request)
	if err != nil || len(leaf.PrivateKey) == 0 || len(leaf.Origins) != 1 {
		t.Fatalf("host transport preparation: %v", err)
	}
	defer clear(leaf.PrivateKey)
	pair, err := tls.X509KeyPair(leaf.Certificate, leaf.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(trust.Certificate)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	request.Origin, request.Placeholders = "https://api.example.com", []string{marker}
	if response, err := client.ResolveSecretProxy(t.Context(), request); err == nil || len(response.Values) > 0 {
		t.Fatal("proxy resolved before exposure")
	}
	delivered, err := client.PreparationSecrets(t.Context(), executor)
	if err != nil || len(delivered.Secrets) != 0 || delivered.Protected == nil || delivered.Protected.Env["TOKEN"] != marker || !bytes.Equal(delivered.Protected.CA, trust.Certificate) {
		t.Fatalf("guest-safe delivery: %v", err)
	}
	raw, err := json.Marshal(delivered)
	if err != nil || bytes.Contains(raw, []byte("protected-first")) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("guest delivery exposed private material")
	}
	if _, err := store.Rotate(t.Context(), f.Environment, secretID, []byte("protected-second"), "rotate-protected"); err != nil {
		t.Fatal(err)
	}
	response, err := client.ResolveSecretProxy(t.Context(), request)
	if err != nil || string(response.Values[marker]) != "protected-first" {
		t.Fatalf("pinned host resolution: %v", err)
	}
	clear(response.Values[marker])
	request.Origin = "https://elsewhere.example.com"
	if response, err := client.ResolveSecretProxy(t.Context(), request); err == nil || len(response.Values) > 0 {
		t.Fatal("wrong origin resolved")
	}
	prepareJSON, _ := json.Marshal(workerapi.SecretProxyRequest{ComputerInstanceID: executor.Identity.InstanceID})
	httpResponse := postAgentComputerJSON(t, server, auth, "/worker/v1/run/secret-proxy/prepare", prepareJSON)
	httpResponse.Body.Close()
	if httpResponse.StatusCode != http.StatusOK || httpResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("host private-key response was cacheable or rejected")
	}
	if err := client.FailPreparation(t.Context(), workerapi.PreparationFailure{Executor: executor, Code: "authored_preparation_failed"}); err != nil {
		t.Fatal(err)
	}
	request.Origin = "https://api.example.com"
	if response, err := client.ResolveSecretProxy(t.Context(), request); err == nil || len(response.Values) > 0 {
		t.Fatal("failed attempt resolved")
	}
	if response, err := client.PrepareSecretProxy(t.Context(), workerapi.SecretProxyRequest{ComputerInstanceID: executor.Identity.InstanceID}); err == nil || len(response.PrivateKey) > 0 {
		clear(response.PrivateKey)
		t.Fatal("failed attempt received a leaf")
	}
}
