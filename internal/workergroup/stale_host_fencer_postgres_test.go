package workergroup

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"ready"}`)) }))
	defer server.Close()
	clock := &advancingFenceClock{now: time.Now()}
	fencer, err := NewStaleHostFencer(fixture.Pool, server.URL,
		WithStaleHostFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleHostFenceClock(clock),
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
	// Returns the connection if an assertion fails before the explicit
	// release; after that release, a second Unlock only reports an error.
	t.Cleanup(func() { _ = holder.Unlock() })
	for range 24 {
		cycle, err := fencer.ReconcileOnce(t.Context())
		if err != nil || !cycle.Suspended {
			t.Fatalf("warming cycle=%+v error=%v", cycle, err)
		}
		clock.now = clock.now.Add(5 * time.Second)
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

	other, err := NewStaleHostFencer(fixture.Pool, server.URL, WithStaleHostFenceClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	otherCycle, err := other.ReconcileOnce(t.Context())
	if err != nil || !otherCycle.Suspended || otherCycle.LockAcquired {
		t.Fatalf("replica inherited serving evidence: %+v %v", otherCycle, err)
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
	t.Cleanup(func() { _ = observer.Unlock() })
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

type expiringFenceTransactions struct {
	pgxStaleHostFenceTransactions
	clock *advancingFenceClock
}

func (tx expiringFenceTransactions) withinStaleHostFenceTransaction(ctx context.Context, apply func(staleHostFenceQueries) error) error {
	return tx.pgxStaleHostFenceTransactions.withinStaleHostFenceTransaction(ctx, func(q staleHostFenceQueries) error {
		return apply(expiringFenceQueries{staleHostFenceQueries: q, clock: tx.clock})
	})
}

type expiringFenceQueries struct {
	staleHostFenceQueries
	clock *advancingFenceClock
}

func (q expiringFenceQueries) RecheckAndFenceStaleWorkerHost(ctx context.Context, params db.RecheckAndFenceStaleWorkerHostParams) (db.RecheckAndFenceStaleWorkerHostRow, error) {
	row, err := q.staleHostFenceQueries.RecheckAndFenceStaleWorkerHost(ctx, params)
	q.clock.now = q.clock.now.Add(11 * time.Second)
	return row, err
}

func TestExpiredServingEvidenceRollsBackHostAndInstanceFences(t *testing.T) {
	fixture := runtest.New(t)
	fixture.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	if _, err := fixture.Pool.Exec(t.Context(), `UPDATE worker_hosts SET observed_at=now()-interval '10 minutes' WHERE id=$1`, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}
	clock := &advancingFenceClock{now: time.Now()}
	fencer, err := newStaleHostFencer(expiringFenceTransactions{pgxStaleHostFenceTransactions: pgxStaleHostFenceTransactions{beginner: fixture.Pool}, clock: clock}, nil, func(context.Context) error { return nil }, WithStaleHostFenceClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	for range 24 {
		if cycle, err := fencer.ReconcileOnce(t.Context()); err != nil || !cycle.Suspended {
			t.Fatalf("warming: %+v %v", cycle, err)
		}
		clock.now = clock.now.Add(5 * time.Second)
	}
	cycle, err := fencer.ReconcileOnce(t.Context())
	if err != nil || !cycle.Suspended || cycle.Fenced != 0 {
		t.Fatalf("expired transaction: %+v %v", cycle, err)
	}
	assertHostStatus(t, fixture, db.WorkerHostStatusActive)
	var ready int
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_instances WHERE worker_host_id=$1 AND observed_state='ready'`, fixture.WorkerID).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	if ready != 1 {
		t.Fatalf("rolled-back instance fence retained: ready=%d", ready)
	}
}
