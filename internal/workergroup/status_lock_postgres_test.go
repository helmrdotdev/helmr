package workergroup

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/pglock"
)

// Group transitions and operator host loss wait for the lifecycle advisory key
// that the worker-group and worker-host CLI commands also take.
func TestStatusMutationsWaitForLifecycleAdvisoryLockPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "current")
	f.activeHost(t, pool, "host-1")
	key := pglock.Key("helmr:worker-group-lifecycle:" + f.groupID().String())
	for name, mutate := range map[string]func() error{
		"pause group": func() error {
			_, err := PauseGroup(t.Context(), f.pool, f.groupID(), f.currentGroup(t).ClaimVersion)
			return err
		},
		"mark host lost": func() error {
			status, err := ReadHostStatus(t.Context(), f.q, f.groupID(), "host-1")
			if err != nil {
				return err
			}
			_, err = MarkHostLost(t.Context(), f.pool, f.groupID(), "host-1", status.ClaimVersion)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			holder, err := f.pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Release()
			if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", key); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- mutate() }()
			waitForAdvisoryLockWaiter(t, f)
			select {
			case err := <-done:
				t.Fatalf("mutation finished while the lifecycle key was held: %v", err)
			default:
			}
			if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", key); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("mutation did not proceed after the lifecycle key was released")
			}
		})
	}
}

func waitForAdvisoryLockWaiter(t *testing.T, f supplyFixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database()))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a transaction to block on the lifecycle advisory key")
}
