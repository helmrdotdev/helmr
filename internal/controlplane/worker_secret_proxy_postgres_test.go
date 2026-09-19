package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestSecretPreparationRequiresActivatedWorker(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var runtimeID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT runtime_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE runtime_instances SET reserved_run_id=$2,reserved_attempt_number=1,reserved_workspace_version_id=(SELECT head_version_id FROM workspaces WHERE id=workspace_id),reservation_expires_at=now()+interval '10 minutes' WHERE id=$1`, runtimeID, work.RunID)
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	const credential = "synthetic-startup-worker-secret"
	hash, err := auth.HashToken(keys.WorkerInstance, credential)
	if err != nil {
		t.Fatal(err)
	}
	serviceID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE worker_instances SET current_service_id=$2,status='registering' WHERE id=$1`, fixture.WorkerID, serviceID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO worker_instance_credentials (id,worker_group_id,worker_instance_id,key_prefix,secret_hash) VALUES ($1,$2,$3,'startup-test',$4)`, uuid.NewV7(), runtest.WorkerGroupID, fixture.WorkerID, hash)
	q := db.New(fixture.Pool)
	store, err := secret.New(q, fixture.Pool, bytes.Repeat([]byte{73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: q, secretProxy: store, authKeys: keys,
		workerTokenSigningKey: bytes.Repeat([]byte{2}, auth.RootKeySize), workerTokenTTL: time.Hour,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	router := chi.NewRouter()
	server.mountWorkerRoutes(router)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	client, err := workerclient.New(httpServer.URL, workerclient.WithAuth(fixture.WorkerID.String(), credential), workerclient.WithService(serviceID.String()))
	if err != nil {
		t.Fatal(err)
	}
	// Token exchange succeeds before qualification; it does not activate the worker.
	if err := client.AuthenticateWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := workerapi.SecretProxyRequest{RuntimeInstanceID: runtimeID.String()}
	if _, err := client.PrepareSecretProxy(t.Context(), request); !httpclient.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("registering worker preparation = %v, want 401", err)
	}
	for _, state := range []string{"active", "draining"} {
		dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE worker_instances SET status=$2,observed_at=now(),draining_at=CASE WHEN $2='draining' THEN now() ELSE NULL END WHERE id=$1`, fixture.WorkerID, state)
		prepared, err := client.PrepareSecretProxy(t.Context(), request)
		if err != nil || len(prepared.Origins) != 0 {
			t.Fatalf("%s worker empty-origin preparation = %+v, %v", state, prepared, err)
		}
	}
	// Even an authorized worker cannot prepare a synthetic, unreserved probe ID.
	if _, err := client.PrepareSecretProxy(t.Context(), workerapi.SecretProxyRequest{RuntimeInstanceID: uuid.NewV7().String()}); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("unreserved runtime preparation = %v, want conflict", err)
	}
}

func TestSecretProxyLiveAuthorityAndWireRotation(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var workspaceID, runtimeID, mountID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT runtime_instances.workspace_id,runtime_instances.id,workspace_mounts.id FROM run_leases JOIN runtime_instances ON runtime_instances.id=run_leases.runtime_instance_id JOIN workspace_mounts ON workspace_mounts.runtime_instance_id=runtime_instances.id WHERE run_leases.run_id=$1`, work.RunID).Scan(&workspaceID, &runtimeID, &mountID); err != nil {
		t.Fatal(err)
	}
	q := db.New(fixture.Pool)
	store, err := secret.New(q, fixture.Pool, bytes.Repeat([]byte{73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), fixture.EnvironmentID, "transport-token", []byte("synthetic-first"), "proxy-create")
	if err != nil {
		t.Fatal(err)
	}
	marker := "hlmr_protected_" + strings.Repeat("a", 64)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO workspace_secrets(workspace_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','GH_TOKEN','protected',ARRAY['https://example.com'],$4)`, workspaceID, fixture.EnvironmentID, created.ID, marker)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE runtime_instances SET reserved_run_id=$2,reserved_attempt_number=1,reserved_workspace_version_id=(SELECT head_version_id FROM workspaces WHERE id=workspace_id),reservation_expires_at=now()+interval '10 minutes' WHERE id=$1`, runtimeID, work.RunID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_mounts SET guest_channel_token_hash='synthetic-channel',guest_channel_token_expires_at=now()+interval '10 minutes' WHERE id=$1`, mountID)
	createTestWorkspaceCA(t, fixture.Pool, store, fixture.EnvironmentID, workspaceID)
	server := &Server{db: q, tx: fixture.Pool, secretProxy: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	actor := workerActor{WorkerInstanceID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}
	invoke := func(resolve bool, who workerActor, request workerapi.SecretProxyRequest, out any) error {
		raw, _ := json.Marshal(request)
		req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, who))
		w := httptest.NewRecorder()
		server.workerSecretProxy(w, req, resolve)
		if w.Code != 200 {
			return errors.New(w.Body.String())
		}
		return json.Unmarshal(w.Body.Bytes(), out)
	}
	request := workerapi.SecretProxyRequest{RuntimeInstanceID: runtimeID.String()}
	var prep workerapi.SecretProxyPreparation
	// A valid reservation can prepare trust while the channel is not authorized.
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_mounts SET guest_channel_token_hash='synthetic-channel',guest_channel_token_expires_at=now()-interval '1 second' WHERE id=$1`, mountID)
	if err := invoke(false, actor, request, &prep); err != nil {
		t.Fatal(err)
	}
	guest, err := workspaceProtectedEnv(t.Context(), q, pgvalue.UUID(fixture.EnvironmentID), pgvalue.UUID(workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	if guest.Env["GH_TOKEN"] != marker || bytes.Contains(guest.CA, []byte("PRIVATE")) {
		t.Fatal("guest delivery contains invalid material")
	}
	leaf, err := tls.X509KeyPair(prep.Certificate, prep.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	var expected atomic.Value
	expected.Store("Bearer synthetic-first")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != expected.Load().(string) {
			t.Error("upstream did not receive the current synthetic credential")
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	proxy, err := secretproxy.New(secretproxy.Config{AllowedDestination: func(netip.Addr) bool { return true }, Origins: prep.Origins, Certificate: func(context.Context, string) (tls.Certificate, error) { return leaf, nil }, UpstreamTLS: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "93.184.216.34:443" {
				return nil, errors.New("unexpected fixture target")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Listener.Addr().String())
		},
		Resolve: func(ctx context.Context, target string, selectors []string) (map[string][]byte, error) {
			var result workerapi.SecretProxyResolution
			err := invoke(true, actor, workerapi.SecretProxyRequest{RuntimeInstanceID: runtimeID.String(), Origin: target, Placeholders: selectors}, &result)
			return result.Values, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go proxy.Serve(&secretDestinationListener{Listener: listener})
	guestRoots := x509.NewCertPool()
	guestRoots.AppendCertsFromPEM(guest.CA)
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	}, TLSClientConfig: &tls.Config{RootCAs: guestRoots}}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	send := func(want int) {
		t.Helper()
		req, _ := http.NewRequest("GET", "https://example.com/", nil)
		req.Header.Set("Authorization", "Bearer "+marker)
		response, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("wire status=%d want=%d", response.StatusCode, want)
		}
	}
	send(502)
	if hits.Load() != 0 {
		t.Fatal("pre-mount request reached upstream")
	}
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_mounts SET guest_channel_token_hash='synthetic-channel',guest_channel_token_expires_at=now()+interval '10 minutes' WHERE id=$1`, mountID)
	send(204)
	if _, err := store.Rotate(t.Context(), fixture.EnvironmentID, pgvalue.MustUUIDValue(created.ID), []byte("synthetic-rotated"), "proxy-rotate"); err != nil {
		t.Fatal(err)
	}
	expected.Store("Bearer synthetic-rotated")
	send(204)
	// Restore/preparation reissues a leaf under exactly the same Workspace root.
	var renewed workerapi.SecretProxyPreparation
	if err := invoke(false, actor, request, &renewed); err != nil {
		t.Fatal(err)
	}
	guestAgain, err := workspaceProtectedEnv(t.Context(), q, pgvalue.UUID(fixture.EnvironmentID), pgvalue.UUID(workspaceID))
	if err != nil || !bytes.Equal(guestAgain.CA, guest.CA) {
		t.Fatal("Workspace public trust changed")
	}
	other := actor
	other.WorkerEpoch++
	if err := invoke(false, other, request, &renewed); err == nil {
		t.Fatal("wrong worker epoch prepared transport")
	}
	selection := workerapi.SecretProxyRequest{RuntimeInstanceID: runtimeID.String(), Origin: "https://example.com", Placeholders: []string{"hlmr_protected_" + strings.Repeat("b", 64)}}
	var resolved workerapi.SecretProxyResolution
	if err := invoke(true, actor, selection, &resolved); err == nil {
		t.Fatal("forged selector resolved")
	}

	// Lease loss must deny even on an already-open guest TLS connection.
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_leases SET expires_at=now()-interval '1 second' WHERE workspace_id=$1`, workspaceID)
	send(502)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_leases SET expires_at=now()+interval '10 minutes' WHERE workspace_id=$1`, workspaceID)
	otherWork := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var otherWorkspace, otherRuntime, otherMount uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT runtime_instances.workspace_id,runtime_instances.id,workspace_mounts.id FROM run_leases JOIN runtime_instances ON runtime_instances.id=run_leases.runtime_instance_id JOIN workspace_mounts ON workspace_mounts.runtime_instance_id=runtime_instances.id WHERE run_leases.run_id=$1`, otherWork.RunID).Scan(&otherWorkspace, &otherRuntime, &otherMount); err != nil {
		t.Fatal(err)
	}
	otherMarker := "hlmr_protected_" + strings.Repeat("c", 64)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO workspace_secrets(workspace_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','GH_TOKEN','protected',ARRAY['https://example.com'],$4)`, otherWorkspace, fixture.EnvironmentID, created.ID, otherMarker)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_mounts SET guest_channel_token_hash='synthetic-other',guest_channel_token_expires_at=now()+interval '10 minutes' WHERE id=$1`, otherMount)
	createTestWorkspaceCA(t, fixture.Pool, store, fixture.EnvironmentID, otherWorkspace)
	var otherPrep workerapi.SecretProxyPreparation
	if err := invoke(false, actor, workerapi.SecretProxyRequest{RuntimeInstanceID: otherRuntime.String()}, &otherPrep); err != nil {
		t.Fatal(err)
	}
	otherSelection := workerapi.SecretProxyRequest{RuntimeInstanceID: otherRuntime.String(), Origin: "https://example.com", Placeholders: []string{marker}}
	if err := invoke(true, actor, otherSelection, &resolved); err == nil {
		t.Fatal("another Workspace used the original selector")
	}
	otherSelection.Placeholders = []string{otherMarker}
	if err := invoke(true, actor, otherSelection, &resolved); err != nil || string(resolved.Values[otherMarker]) != "synthetic-rotated" {
		t.Fatal("own Workspace selector failed")
	}
	for _, value := range resolved.Values {
		clear(value)
	}
	if hits.Load() != 2 {
		t.Fatal("denied requests contacted upstream")
	}
	if _, err := store.Revoke(t.Context(), fixture.EnvironmentID, pgvalue.MustUUIDValue(created.ID), "proxy-revoke"); err != nil {
		t.Fatal(err)
	}
	send(502)
	if hits.Load() != 2 {
		t.Fatalf("unexpected upstream count %d", hits.Load())
	}
}

// Portable protocol fixture supplies the socket fact that Linux TPROXY owns.
// The privileged firecracker fixture separately proves original destination.
type secretDestinationListener struct{ net.Listener }

func (l *secretDestinationListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return &secretDestinationConn{c}, nil
}

type secretDestinationConn struct{ net.Conn }

func (c *secretDestinationConn) LocalAddr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort("93.184.216.34:443"))
}
