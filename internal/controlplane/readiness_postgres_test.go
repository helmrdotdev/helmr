package controlplane

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReadinessRejectsReadOnlyServingPool(t *testing.T) {
	fixture := agenttest.New(t)
	cfg := fixture.Pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	readOnly, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	for _, test := range []struct {
		name   string
		pool   *pgxpool.Pool
		status int
	}{
		{"read only", readOnly, http.StatusServiceUnavailable},
		{"writable", fixture.Pool, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{readinessDB: test.pool, log: slog.Default()}
			response := httptest.NewRecorder()
			server.readyz(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("readiness response is cacheable")
			}
			if response.Code != test.status {
				t.Fatalf("readiness = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

type servingTestClock struct{ now time.Time }

func (c *servingTestClock) Now() time.Time { return c.now }
func (c *servingTestClock) Wait(ctx context.Context, delay time.Duration) error {
	c.now = c.now.Add(delay)
	return ctx.Err()
}

// The real HTTP/authentication and PostgreSQL paths run with a controlled
// dispatcher clock. This does not replace a wall-clock deployment outage test.
func TestServingOutagePreservesHostsUntilRecovery(t *testing.T) {
	for _, outage := range []string{"HTTP unavailable", "CP database read only"} {
		for _, survives := range []bool{true, false} {
			name := outage + "/dead host"
			if survives {
				name = outage + "/live draining host"
			}
			t.Run(name, func(t *testing.T) {
				fixture := agenttest.New(t)
				secret := seedHostSecret(t, fixture.Pool, fixture.Worker)
				cfg := fixture.Pool.Config()
				cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
				readOnly, err := pgxpool.NewWithConfig(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer readOnly.Close()
				healthyHandler := newPostgresServer(t, fixture.Pool, func(cfg *ServerConfig) { cfg.ReadinessDB = fixture.Pool })
				readOnlyHandler := newPostgresServer(t, readOnly, func(cfg *ServerConfig) { cfg.ReadinessDB = readOnly })
				var unavailable atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if unavailable.Load() {
						if outage == "HTTP unavailable" {
							http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
							return
						}
						readOnlyHandler.ServeHTTP(w, r)
						return
					}
					healthyHandler.ServeHTTP(w, r)
				}))
				defer server.Close()
				client := secret.client(t, server.URL)
				if survives {
					if _, err := client.DrainWorker(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := client.ObserveWorker(t.Context(), workerapi.Observation{}); err != nil {
					t.Fatal(err)
				}
				clock := &servingTestClock{now: time.Now()}
				fencer, err := workergroup.NewStaleHostFencer(fixture.Pool, server.URL, workergroup.WithStaleHostFenceClock(clock), workergroup.WithStaleHostFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
				if err != nil {
					t.Fatal(err)
				}
				cycle := func(wantSuspended bool, wantFenced int) {
					t.Helper()
					result, err := fencer.ReconcileOnce(t.Context())
					if err != nil || result.Suspended != wantSuspended || result.Fenced != wantFenced {
						t.Fatalf("cycle=%+v error=%v want suspended=%v fenced=%d", result, err, wantSuspended, wantFenced)
					}
					clock.now = clock.now.Add(5 * time.Second)
				}
				for range 24 {
					cycle(true, 0)
				}
				cycle(false, 0)
				unavailable.Store(true)
				dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE worker_hosts SET observed_at=now()-interval '10 minutes' WHERE id=$1`, fixture.Worker)
				if _, err := client.ObserveWorker(t.Context(), workerapi.Observation{}); err == nil {
					t.Fatal("observation unexpectedly succeeded during outage")
				}
				// Longer than the old stale-fence deadline, even though Dispatcher can write.
				for range 26 {
					cycle(true, 0)
				}
				assertServingHostPreserved(t, fixture, survives)
				unavailable.Store(false)
				for i := range 24 {
					if survives && i == 12 {
						if _, err := client.ObserveWorker(t.Context(), workerapi.Observation{}); err != nil {
							t.Fatalf("live host could not observe after recovery: %v", err)
						}
					}
					cycle(true, 0)
				}
				if survives {
					cycle(false, 0)
					assertServingHostPreserved(t, fixture, true)
				} else {
					cycle(false, 1)
					var lost, revoked bool
					if err := fixture.Pool.QueryRow(t.Context(), `SELECT status='lost', NOT EXISTS(SELECT 1 FROM worker_host_secrets WHERE worker_host_id=$1 AND revoked_at IS NULL) FROM worker_hosts WHERE id=$1`, fixture.Worker).Scan(&lost, &revoked); err != nil {
						t.Fatal(err)
					}
					if !lost || !revoked {
						t.Fatalf("dead host not fenced after recovery: lost=%v revoked=%v", lost, revoked)
					}
					// A missed heartbeat does not prove the VM stopped; retain physical charges.
					var retainedLeases int
					if err := fixture.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE worker_host_id=$1 AND fenced_at IS NULL`, fixture.Worker).Scan(&retainedLeases); err != nil {
						t.Fatal(err)
					}
					if retainedLeases != 1 {
						t.Fatalf("retained physical leases=%d", retainedLeases)
					}
				}
			})
		}
	}
}

func assertServingHostPreserved(t *testing.T, fixture agenttest.Fixture, draining bool) {
	t.Helper()
	want := "active"
	if draining {
		want = "draining"
	}
	var status string
	var secrets, instances int
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT status, (SELECT count(*) FROM worker_host_secrets WHERE worker_host_id=$1 AND revoked_at IS NULL), (SELECT count(*) FROM computer_leases WHERE worker_host_id=$1 AND fenced_at IS NULL) FROM worker_hosts WHERE id=$1`, fixture.Worker).Scan(&status, &secrets, &instances); err != nil {
		t.Fatal(err)
	}
	if status != want || secrets != 1 || instances != 1 {
		t.Fatalf("source not preserved: status=%s secrets=%d ready instances=%d", status, secrets, instances)
	}
}
