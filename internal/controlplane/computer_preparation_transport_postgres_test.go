package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

// switchPrimaryPool moves the Group's primary pool away from the host's pool,
// which advances only the Group claim version.
func switchPrimaryPool(t *testing.T, f runtest.Fixture) func(context.Context) error {
	t.Helper()
	standby := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_pools (id,worker_group_id,name,status,vm_platform_id,
 capacity_cpu_millis,capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,
 per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,sealed_at)
 SELECT $2::uuid,worker_group_id,'standby-'||$2::uuid::text,'active',vm_platform_id,capacity_cpu_millis,capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,
 per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,sealed_at FROM worker_pools WHERE id=$1`, f.WorkerPoolID, standby)
	return func(ctx context.Context) error {
		var claim int64
		if err := f.Pool.QueryRow(ctx, `SELECT claim_version FROM worker_groups WHERE id=$1`, runtest.WorkerGroupID).Scan(&claim); err != nil {
			return err
		}
		_, err := db.New(f.Pool).SetWorkerGroupPrimaryPool(ctx, db.SetWorkerGroupPrimaryPoolParams{
			PoolID: pgvalue.UUID(standby), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedGroupClaimVersion: claim,
		})
		return err
	}
}

// transitionTx commits an armed transition once, when the served handler
// begins its first transaction: after authentication, which reads through
// the server's query database, and before any authority lock.
type transitionTx struct {
	db.TxDB
	mu      sync.Mutex
	pending func(context.Context) error
}

func (d *transitionTx) arm(transition func(context.Context) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = transition
}

func (d *transitionTx) Begin(ctx context.Context) (pgx.Tx, error) {
	d.mu.Lock()
	transition := d.pending
	d.pending = nil
	d.mu.Unlock()
	if transition != nil {
		if err := transition(ctx); err != nil {
			return nil, err
		}
	}
	return d.TxDB.Begin(ctx)
}

// newServedClaimsRace serves the fixture's control plane through NewServer
// with real token exchange and worker authentication. The first request to
// the raced route commits the transition after authentication accepted it and
// before the handler takes its authority locks.
func newServedClaimsRace(t *testing.T, f initialPublicationFixture, raced string, transition func(context.Context) error) *workerClaimsRace {
	t.Helper()
	tx := &transitionTx{TxDB: f.Pool}
	handler := f.serve(t, func(cfg *ServerConfig) { cfg.TX = tx })
	race := &workerClaimsRace{statuses: map[string][]int{}, bodies: map[string][][]byte{}}
	pending := transition
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		race.mu.Lock()
		defer race.mu.Unlock()
		if r.URL.Path == "/worker/v1/instance/token" {
			race.tokens++
			handler.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		race.bodies[r.URL.Path] = append(race.bodies[r.URL.Path], body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.URL.Path == raced && pending != nil {
			tx.arm(pending)
			pending = nil
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		tx.arm(nil)
		race.statuses[r.URL.Path] = append(race.statuses[r.URL.Path], response.Code)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	t.Cleanup(httpServer.Close)
	race.client = seedHostCredential(t, f.Pool, f.worker.HostID).client(t, httpServer.URL)
	return race
}

func TestInitialComputerPreparationReauthenticatesAcrossPrimaryPoolSwitch(t *testing.T) {
	const (
		keyPath        = "/worker/v1/run/computer-instances/initialization/key"
		registerPath   = "/worker/v1/run/computer-instances/initialization/objects/register"
		certifyPath    = "/worker/v1/run/computer-instances/initialization/objects/certify"
		generationPath = "/worker/v1/run/computer-instances/initialization/generation"
	)
	for name, raced := range map[string]string{"key": keyPath, "object registration": registerPath, "object certification": certifyPath, "generation publication": generationPath} {
		t.Run(name, func(t *testing.T) {
			f := newInitialPublicationFixture(t)
			race := newServedClaimsRace(t, f, raced, switchPrimaryPool(t, f.Fixture))
			_, _, published := f.publishInitialGeneration(t, race.client, oci.RuntimeConfig{User: "root"})
			race.requireReplayed(t, raced)
			var head string
			if err := f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id::text FROM computers c JOIN computer_instances i ON i.computer_id=c.id WHERE i.id=$1`, f.runtime).Scan(&head); err != nil || head != published.VersionID {
				t.Fatalf("published head=%s response=%s err=%v", head, published.VersionID, err)
			}
		})
	}
}

var errInjectedClaimRead = errors.New("injected Worker claim read failure")

// claimReadFaults fails the worker claim read once armed. Authority locks
// still run, so the injected failure reaches exactly the post-lock claim
// comparison.
type claimReadFaults struct {
	db.TxDB
	armed atomic.Bool
}

func (f *claimReadFaults) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.TxDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return claimReadFaultTx{Tx: tx, faults: f}, nil
}

type claimReadFaultTx struct {
	pgx.Tx
	faults *claimReadFaults
}

func (t claimReadFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.faults.armed.Load() && strings.HasPrefix(sql, "SELECT w.claim_version,g.claim_version") {
		return claimReadFaultRow{}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

type claimReadFaultRow struct{}

func (claimReadFaultRow) Scan(...any) error { return errInjectedClaimRead }

// armingKeyWrapper arms the claim-read fault during a provider unwrap once
// enabled, so the first authority read succeeds and only the final
// revalidation fails. It keeps the plaintext the provider returned.
type armingKeyWrapper struct {
	computer.KeyWrapper
	faults   *claimReadFaults
	enabled  atomic.Bool
	returned [][]byte
}

func (w *armingKeyWrapper) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	key, err := w.KeyWrapper.Unwrap(ctx, scope, id, e)
	if w.enabled.Load() {
		w.returned = append(w.returned, key)
		w.faults.armed.Store(true)
	}
	return key, err
}

// serveClaimReadFaults serves the fixture with the claim-read fault and its
// arming provider.
func serveClaimReadFaults(t *testing.T, f initialPublicationFixture) (http.Handler, *armingKeyWrapper) {
	t.Helper()
	faults := &claimReadFaults{TxDB: f.Pool}
	wrapper := &armingKeyWrapper{KeyWrapper: f.keys, faults: faults}
	handler := f.serve(t, func(cfg *ServerConfig) {
		cfg.TX = faults
		cfg.ComputerKeys = wrapper
	})
	return handler, wrapper
}

func postWorker(t *testing.T, handler http.Handler, token, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requireClearedPlaintext(t *testing.T, returned [][]byte) {
	t.Helper()
	if len(returned) == 0 {
		t.Fatal("final revalidation was not reached after provider unwrap")
	}
	for _, key := range returned {
		if len(key) == 0 || !bytes.Equal(key, make([]byte, len(key))) {
			t.Fatal("plaintext not cleared after final claim read failure")
		}
	}
}

// A database failure during the final revalidation of initial key delivery
// stays retryable unavailability, and the unwrapped plaintext is cleared.
func TestInitialComputerKeyFinalClaimReadFailureIsUnavailable(t *testing.T) {
	f := newInitialPublicationFixture(t)
	handler, wrapper := serveClaimReadFaults(t, f)
	token := seedHostCredential(t, f.Pool, f.worker.HostID).token(t, handler)
	wrapper.enabled.Store(true)
	response := postWorker(t, handler, token, "/worker/v1/run/computer-instances/initialization/key", workerapi.InitialComputerKeyRequest{ComputerInstanceID: pgvalue.UUIDString(f.runtime), DesiredVersion: 1})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("final claim read failure status=%d body=%s", response.Code, response.Body.String())
	}
	requireClearedPlaintext(t, wrapper.returned)
}

// A database failure during the final revalidation of source delivery stays
// retryable unavailability, and every unwrapped plaintext is cleared.
func TestComputerSourceFinalClaimReadFailureIsUnavailable(t *testing.T) {
	f := newInitialPublicationFixture(t)
	handler, wrapper := serveClaimReadFaults(t, f)
	credential := seedHostCredential(t, f.Pool, f.worker.HostID)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f.publishInitialGeneration(t, credential.client(t, server.URL), oci.RuntimeConfig{User: "root"})
	token := credential.token(t, handler)
	wrapper.enabled.Store(true)
	response := postWorker(t, handler, token, "/worker/v1/run/computer-instances/computer-source", workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(f.runtime), DesiredVersion: 1})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("final source claim read failure status=%d body=%s", response.Code, response.Body.String())
	}
	requireClearedPlaintext(t, wrapper.returned)
}
