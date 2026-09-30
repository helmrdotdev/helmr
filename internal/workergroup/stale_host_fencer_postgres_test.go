package workergroup

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestStaleHostFencerSkipsCycleWhileLockIsHeld(t *testing.T) {
	fixture := runtest.New(t)
	if _, err := fixture.Pool.Exec(t.Context(),
		`UPDATE worker_hosts SET observed_at = now() - interval '10 minutes' WHERE id = $1`,
		fixture.WorkerID,
	); err != nil {
		t.Fatal(err)
	}
	fencer, err := NewStaleHostFencer(fixture.Pool,
		WithStaleHostFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleHostFenceClock(fixedStaleHostFenceClock{now: time.Now()}),
	)
	if err != nil {
		t.Fatal(err)
	}

	holder, locked, err := pglock.TryAcquire(t.Context(), fixture.Pool, pglock.Key(staleHostFenceLockName))
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("expected to hold the stale host fence lock")
	}
	cycle, err := fencer.ReconcileOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.LockAcquired || cycle.Selected != 0 || len(cycle.Results) != 0 {
		t.Fatalf("cycle while lock is held = %+v, want skipped cycle", cycle)
	}
	assertHostStatus(t, fixture, db.WorkerHostStatusActive)
	if err := holder.Unlock(); err != nil {
		t.Fatal(err)
	}

	cycle, err = fencer.ReconcileOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !cycle.LockAcquired || cycle.Selected != 1 || cycle.Fenced != 1 || cycle.Results[0].WorkerHostID != pgvalue.UUID(fixture.WorkerID) {
		t.Fatalf("cycle after release = %+v, want the stale host fenced", cycle)
	}
	assertHostStatus(t, fixture, db.WorkerHostStatusLost)

	observer, locked, err := pglock.TryAcquire(t.Context(), fixture.Pool, pglock.Key(staleHostFenceLockName))
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("fencer kept the stale host fence lock after its cycle")
	}
	if err := observer.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func assertHostStatus(t *testing.T, fixture runtest.Fixture, want db.WorkerHostStatus) {
	t.Helper()
	var status db.WorkerHostStatus
	if err := fixture.Pool.QueryRow(t.Context(),
		`SELECT status FROM worker_hosts WHERE id = $1`, fixture.WorkerID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != want {
		t.Fatalf("worker host status = %q, want %q", status, want)
	}
}
