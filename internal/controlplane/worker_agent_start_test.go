package controlplane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type sessionBundleStore struct {
	*bundleUploadStoreFixture
	body   []byte
	onRead func()
}

func (s *sessionBundleStore) Get(context.Context, string) (io.ReadCloser, error) {
	if s.onRead != nil {
		s.onRead()
	}
	return io.NopCloser(bytes.NewReader(s.body)), nil
}
func TestWorkerAgentStartBindsPinnedBundleAndRechecksAfterRead(t *testing.T) {
	for _, mode := range []string{"current", "bad bytes", "hold during read", "stale attachment"} {
		t.Run(mode, func(t *testing.T) {
			f := agenttest.New(t)
			raw, manifest := controlPlaneDeploymentBundle(t)
			digest, err := bundle.Digest(raw)
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployments SET bundle_digest=$1 WHERE environment_id=$2 AND id=$3`, digest, f.Environment, f.Deployment)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.Session)
			store := &sessionBundleStore{bundleUploadStoreFixture: &bundleUploadStoreFixture{objects: map[string]cas.Object{digest: {Digest: digest, SizeBytes: int64(len(raw)), MediaType: bundle.MediaType}}}, body: raw}
			if mode == "bad bytes" {
				store.body = bytes.Repeat([]byte("!"), len(raw))
			}
			if mode == "hold during read" {
				store.onRead = func() {
					dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,gen_random_uuid(),$2,'local','test')`, f.Environment, f.Session)
				}
			}
			server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.CAS = store }))
			defer server.Close()
			client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
			session := runtimeTestSession(f)
			attachment, err := client.AcquireAgentAttachment(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			request := workerapi.AgentControlRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence}
			if mode == "stale attachment" {
				request.AttachmentSequence++
			}
			response, err := client.AuthorizeAgentStart(t.Context(), request)
			switch mode {
			case "current":
				if err != nil || response.Program.DeploymentID != f.Deployment.String() || response.Program.Artifact.Digest != manifest.Program.Artifact.Digest || response.Program.Runtime.Digest != manifest.Runtime.Artifact.Digest || response.ComputerID != f.Computer.String() || response.Authority.AuthorityGeneration != 1 || !response.Authority.ExpiresAt.After(time.Now()) {
					t.Fatalf("start: %+v %v", response, err)
				}
				// A final release uses the immutable pin and current business
				// authority even when object storage is unavailable afterward.
				store.body = bytes.Repeat([]byte("!"), len(raw))
				release := workerapi.AgentStartReleaseRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence, BundleDigest: response.BundleDigest}
				if _, err = client.ReleaseAgentStart(t.Context(), release); err != nil {
					t.Fatalf("release reread unavailable bundle: %v", err)
				}
				release.BundleDigest = "sha256:" + strings.Repeat("0", 64)
				if _, err = client.ReleaseAgentStart(t.Context(), release); !httpclient.IsStatus(err, http.StatusConflict) {
					t.Fatalf("changed release pin: %v", err)
				}
				release.BundleDigest = response.BundleDigest
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,gen_random_uuid(),$2,'local','test')`, f.Environment, f.Session)
				if _, err = client.ReleaseAgentStart(t.Context(), release); !httpclient.IsStatus(err, http.StatusConflict) {
					t.Fatalf("held release: %v", err)
				}
			case "bad bytes":
				if !httpclient.IsStatus(err, http.StatusServiceUnavailable) {
					t.Fatalf("bad bytes: %v", err)
				}
			default:
				if !httpclient.IsStatus(err, http.StatusConflict) {
					t.Fatalf("unavailable setup: %v", err)
				}
			}
		})
	}
}

func TestWorkerAgentStartDeliversProcessPinnedSecrets(t *testing.T) {
	f := agenttest.New(t)
	raw, _ := controlPlaneDeploymentBundle(t)
	digest, err := bundle.Digest(raw)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployments SET bundle_digest=$1 WHERE environment_id=$2 AND id=$3`, digest, f.Environment, f.Deployment)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.Session)
	secrets, err := secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	record, err := secrets.Create(t.Context(), f.Environment, "START_TOKEN", []byte("first-value"), "start-create")
	if err != nil {
		t.Fatal(err)
	}
	id := pgvalue.MustUUIDValue(record.ID)
	for _, binding := range []struct{ kind, target, mode string }{{"env", "TOKEN", "protected"}, {"env", "RAW_TOKEN", "raw"}, {"file", "/secrets/token", "raw"}} {
		marker, err := secretbinding.Placeholder(binding.mode)
		if err != nil {
			t.Fatal(err)
		}
		origins := []string{}
		if binding.mode == "protected" {
			origins = append(origins, "https://api.example.com")
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`, f.Environment, f.Computer, id, binding.kind, binding.target, binding.mode, origins, marker)
	}
	trust, err := secrets.GenerateProxyTrust(f.Environment, f.Computer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET proxy_ca_certificate=$3,proxy_ca_not_after=$4,proxy_ca_private_key_nonce=$5,proxy_ca_private_key_ciphertext=$6 WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer, trust.Certificate, trust.NotAfter, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext)
	store := &sessionBundleStore{bundleUploadStoreFixture: &bundleUploadStoreFixture{objects: map[string]cas.Object{digest: {Digest: digest, SizeBytes: int64(len(raw)), MediaType: bundle.MediaType}}}, body: raw}
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.CAS = store; cfg.SecretDelivery = secrets; cfg.SecretProxy = secrets }))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	attachment, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentControlRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence}
	first, err := client.AuthorizeAgentStart(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertDelivery := func(response workerapi.AgentStartResponse) {
		t.Helper()
		if len(response.Secrets) != 2 || response.ProtectedEnv == nil || len(response.ProtectedEnv.Env) != 1 || !bytes.Equal(response.ProtectedEnv.CA, trust.Certificate) {
			t.Fatal("incomplete startup materials")
		}
		seenEnv, seenFile := false, false
		for _, delivery := range response.Secrets {
			if string(delivery.Value) != "first-value" {
				t.Fatal("startup changed pinned raw value")
			}
			if delivery.Env != nil && delivery.Env.Name == "RAW_TOKEN" {
				seenEnv = true
			}
			if delivery.File != nil && delivery.File.Path == "/secrets/token" {
				seenFile = true
			}
		}
		if !seenEnv || !seenFile || !strings.HasPrefix(response.ProtectedEnv.Env["TOKEN"], "hlmr_protected_") {
			t.Fatal("wrong startup placements")
		}
	}
	assertDelivery(first)
	if _, err = secrets.Rotate(t.Context(), f.Environment, id, []byte("second-value"), "start-rotate"); err != nil {
		t.Fatal(err)
	}
	retry, err := client.AuthorizeAgentStart(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertDelivery(retry)
	if retry.ProtectedEnv.Env["TOKEN"] != first.ProtectedEnv.Env["TOKEN"] {
		t.Fatal("retry changed selector")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND session_id=$2`, f.Environment, f.Session).Scan(&count); err != nil || count != 1 {
		t.Fatalf("logical exposure count %d: %v", count, err)
	}
	if _, err = secrets.Revoke(t.Context(), f.Environment, id, "start-revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.AuthorizeAgentStart(t.Context(), request); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("revoked startup: %v", err)
	}
	release := workerapi.AgentStartReleaseRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence, BundleDigest: digest}
	if _, err = client.ReleaseAgentStart(t.Context(), release); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("revoked release: %v", err)
	}
}
