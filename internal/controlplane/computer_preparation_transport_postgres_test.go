package controlplane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
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
// with real host credential issue and worker authentication. The first request to
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
		if r.URL.Path == "/worker/v1/instance/credential" {
			race.hostCredentials++
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
	race.client = seedHostSecret(t, f.Pool, f.worker.HostID).client(t, httpServer.URL)
	return race
}

func TestInitialComputerPreparationReauthenticatesAcrossPrimaryPoolSwitch(t *testing.T) {
	const (
		keyPath      = "/worker/v1/run/computer-instances/initialization/seed"
		registerPath = "/worker/v1/run/computer-instances/initialization/objects/register"
		certifyPath  = "/worker/v1/run/computer-instances/initialization/objects/certify"
		versionPath  = "/worker/v1/run/computer-instances/initialization/version"
	)
	for name, raced := range map[string]string{"seed": keyPath, "object registration": registerPath, "object certification": certifyPath, "version publication": versionPath} {
		t.Run(name, func(t *testing.T) {
			f := newInitialPublicationFixture(t)
			race := newServedClaimsRace(t, f, raced, switchPrimaryPool(t, f.Fixture))
			_, _, published := f.publishInitialVersion(t, race.client)
			race.requireReplayed(t, raced)
			var head string
			if err := f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id::text FROM computers c JOIN computer_instances i ON i.computer_id=c.id WHERE i.id=$1`, f.instance).Scan(&head); err != nil || head != published.VersionID {
				t.Fatalf("published head=%s response=%s err=%v", head, published.VersionID, err)
			}
		})
	}
}
