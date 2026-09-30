package workergroup

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"

	"github.com/jackc/pgx/v5/pgtype"
)

var (
	staleHostRunGroupID        = pgvalue.UUID(uuid.MustParse("01900000-0000-7000-8000-000000000803"))
	staleHostManagedGroupID    = pgvalue.UUID(uuid.MustParse("01900000-0000-7000-8000-000000000804"))
	staleHostSelfHostedGroupID = pgvalue.UUID(uuid.MustParse("01900000-0000-7000-8000-000000000805"))
)

func TestStaleHostFencerFencesStaleActiveWorker(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleHostCandidate(1, db.WorkerHostStatusActive, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleHostFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate}}
	fencer := newTestStaleHostFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Selected != 1 || cycle.Fenced != 1 || cycle.Skipped != 0 {
		t.Fatalf("cycle = %+v, want one fenced worker", cycle)
	}
	if len(store.rechecks) != 1 || store.rechecks[0].ExpectedEpoch != candidate.CurrentEpoch {
		t.Fatalf("rechecks = %+v, want selected epoch", store.rechecks)
	}
	if got := store.rechecks[0].ReasonCode.String; got != staleHostReasonCode {
		t.Fatalf("reason code = %q, want %q", got, staleHostReasonCode)
	}
}

func TestStaleHostFencerExcludesFreshDisabledAndLostWorkers(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	store := &fakeStaleHostFenceQueries{}
	fencer := newTestStaleHostFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Selected != 0 || cycle.Fenced != 0 || len(store.rechecks) != 0 {
		t.Fatalf("cycle = %+v rechecks = %+v, want no eligible workers", cycle, store.rechecks)
	}
	if len(store.listParams) != 1 || store.listParams[0].WorkerGroupID.Valid || store.listParams[0].RowLimit != DefaultStaleHostFenceBatch {
		t.Fatalf("list params = %+v, want one unscoped batch", store.listParams)
	}
	if got := store.listParams[0].RegistrationStaleBefore.Time; !got.Equal(now.Add(-DefaultWorkerRegistrationReadinessGrace)) {
		t.Fatalf("registration stale cutoff = %v, want %v", got, now.Add(-DefaultWorkerRegistrationReadinessGrace))
	}
}

func TestStaleHostFencerLateFreshObservationWinsRecheck(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleHostCandidate(2, db.WorkerHostStatusActive, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleHostFenceQueries{
		candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate},
		recheck: func(context.Context, db.RecheckAndFenceStaleWorkerHostParams) (db.RecheckAndFenceStaleWorkerHostRow, error) {
			return db.RecheckAndFenceStaleWorkerHostRow{}, pgx.ErrNoRows
		},
	}
	fencer := newTestStaleHostFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Fenced != 0 || cycle.Skipped != 1 {
		t.Fatalf("cycle = %+v, want late observation to skip fence", cycle)
	}
	if got := cycle.Results[0].Reason; got != "fresh_observation_or_worker_changed" {
		t.Fatalf("skip reason = %q", got)
	}
}

func TestStaleHostFencerOldEpochCannotFenceNewEpoch(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleHostCandidate(3, db.WorkerHostStatusDraining, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleHostFenceQueries{
		candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate},
		recheck: func(_ context.Context, params db.RecheckAndFenceStaleWorkerHostParams) (db.RecheckAndFenceStaleWorkerHostRow, error) {
			if params.ExpectedEpoch.Int64 != 7 {
				t.Fatalf("expected epoch = %d, want selected epoch 7", params.ExpectedEpoch.Int64)
			}
			return db.RecheckAndFenceStaleWorkerHostRow{}, pgx.ErrNoRows
		},
	}
	fencer := newTestStaleHostFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Fenced != 0 || cycle.Skipped != 1 {
		t.Fatalf("cycle = %+v, want changed epoch skipped", cycle)
	}
}

func TestStaleHostFencerHandlesRegisteringWorkerWithoutObservation(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleHostCandidate(4, db.WorkerHostStatusRegistering, now.Add(-time.Minute), "registering_observation_missing")
	store := &fakeStaleHostFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate}}
	fencer := newTestStaleHostFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Fenced != 1 || cycle.Results[0].Reason != "registering_observation_missing" {
		t.Fatalf("cycle = %+v, want missing registering observation fenced", cycle)
	}
}

func TestStaleHostFencerIsDeploymentModeAgnostic(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	managed := staleHostCandidate(5, db.WorkerHostStatusActive, now.Add(-time.Minute), "worker_observation_stale")
	managed.WorkerGroupID = staleHostManagedGroupID
	selfHosted := staleHostCandidate(6, db.WorkerHostStatusActive, now.Add(-time.Minute), "worker_observation_stale")
	selfHosted.WorkerGroupID = staleHostSelfHostedGroupID
	store := &fakeStaleHostFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{managed, selfHosted}}
	fencer := newTestStaleHostFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Fenced != 2 {
		t.Fatalf("cycle = %+v, want both groups fenced by the same path", cycle)
	}
	if store.rechecks[0].WorkerGroupID == store.rechecks[1].WorkerGroupID {
		t.Fatalf("rechecks = %+v, want distinct groups", store.rechecks)
	}
}

func TestStaleHostFencerPersistentFailureRetriesUntilCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := &cancelingStaleHostFenceClock{
		now:       time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		cancel:    cancel,
		cancelAt:  4,
		waitCalls: &atomic.Int32{},
	}
	transactions := &failingStaleHostFenceTransactions{err: errors.New("database unavailable")}
	fencer, err := newStaleHostFencer(
		transactions, nil,
		WithStaleHostFenceInterval(time.Millisecond),
		WithStaleHostFenceTimeout(time.Second),
		WithStaleHostFenceMaxBackoff(8*time.Millisecond),
		WithStaleHostFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleHostFenceClock(clock),
	)
	if err != nil {
		t.Fatal(err)
	}

	err = fencer.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if got := transactions.calls.Load(); got != 4 {
		t.Fatalf("transaction attempts = %d, want 4", got)
	}
	if got, want := clock.delays(), []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond}; !equalDurations(got, want) {
		t.Fatalf("retry delays = %v, want %v", got, want)
	}
}

func TestStaleHostFenceFailureRollsBackReportedResults(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	first := staleHostCandidate(7, db.WorkerHostStatusActive, now.Add(-time.Minute), "worker_observation_stale")
	second := staleHostCandidate(8, db.WorkerHostStatusActive, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleHostFenceQueries{
		candidates: []db.ListStaleWorkerFenceCandidatesRow{first, second},
		recheck: func(_ context.Context, params db.RecheckAndFenceStaleWorkerHostParams) (db.RecheckAndFenceStaleWorkerHostRow, error) {
			if params.ID == second.ID {
				return db.RecheckAndFenceStaleWorkerHostRow{}, errors.New("write failed")
			}
			return db.RecheckAndFenceStaleWorkerHostRow{ID: params.ID}, nil
		},
	}
	fencer := newTestStaleHostFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err == nil {
		t.Fatal("ReconcileOnce unexpectedly succeeded")
	}
	if cycle.Selected != 0 || cycle.Fenced != 0 || len(cycle.Results) != 0 {
		t.Fatalf("cycle = %+v, must not report rolled-back fences", cycle)
	}
}

func newTestStaleHostFencer(t *testing.T, store *fakeStaleHostFenceQueries, now time.Time) *StaleHostFencer {
	t.Helper()
	fencer, err := newStaleHostFencer(
		fakeStaleHostFenceTransactions{queries: store}, nil,
		WithStaleHostFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleHostFenceClock(fixedStaleHostFenceClock{now: now}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return fencer
}

func staleHostCandidate(seed byte, state db.WorkerHostStatus, freshness time.Time, reason string) db.ListStaleWorkerFenceCandidatesRow {
	var id [16]byte
	id[15] = seed
	return db.ListStaleWorkerFenceCandidatesRow{
		ID:            pgtype.UUID{Bytes: id, Valid: true},
		WorkerGroupID: staleHostRunGroupID,
		CurrentEpoch:  pgtype.Int8{Int64: 7, Valid: true},
		Status:        state,
		FreshnessAt:   pgtype.Timestamptz{Time: freshness, Valid: true},
		Reason:        reason,
	}
}

type fakeStaleHostFenceTransactions struct {
	queries staleHostFenceQueries
}

func (transactions fakeStaleHostFenceTransactions) withinStaleHostFenceTransaction(
	_ context.Context,
	fn func(staleHostFenceQueries) error,
) error {
	return fn(transactions.queries)
}

type fakeStaleHostFenceQueries struct {
	candidates []db.ListStaleWorkerFenceCandidatesRow
	listParams []db.ListStaleWorkerFenceCandidatesParams
	rechecks   []db.RecheckAndFenceStaleWorkerHostParams
	recheck    func(context.Context, db.RecheckAndFenceStaleWorkerHostParams) (db.RecheckAndFenceStaleWorkerHostRow, error)
}

func (queries *fakeStaleHostFenceQueries) ListStaleWorkerFenceCandidates(
	_ context.Context,
	params db.ListStaleWorkerFenceCandidatesParams,
) ([]db.ListStaleWorkerFenceCandidatesRow, error) {
	queries.listParams = append(queries.listParams, params)
	candidates := make([]db.ListStaleWorkerFenceCandidatesRow, 0, len(queries.candidates))
	for _, candidate := range queries.candidates {
		candidates = append(candidates, candidate)
		if len(candidates) == int(params.RowLimit) {
			break
		}
	}
	return candidates, nil
}

func (queries *fakeStaleHostFenceQueries) RecheckAndFenceStaleWorkerHost(
	ctx context.Context,
	params db.RecheckAndFenceStaleWorkerHostParams,
) (db.RecheckAndFenceStaleWorkerHostRow, error) {
	queries.rechecks = append(queries.rechecks, params)
	if queries.recheck != nil {
		return queries.recheck(ctx, params)
	}
	return db.RecheckAndFenceStaleWorkerHostRow{
		ID: params.ID, WorkerGroupID: params.WorkerGroupID, CurrentEpoch: params.ExpectedEpoch,
	}, nil
}

type failingStaleHostFenceTransactions struct {
	err   error
	calls atomic.Int32
}

func (transactions *failingStaleHostFenceTransactions) withinStaleHostFenceTransaction(
	context.Context,
	func(staleHostFenceQueries) error,
) error {
	transactions.calls.Add(1)
	return transactions.err
}

type fixedStaleHostFenceClock struct {
	now time.Time
}

func (clock fixedStaleHostFenceClock) Now() time.Time { return clock.now }

func (fixedStaleHostFenceClock) Wait(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

type cancelingStaleHostFenceClock struct {
	now       time.Time
	cancel    context.CancelFunc
	cancelAt  int32
	waitCalls *atomic.Int32
	mu        sync.Mutex
	waits     []time.Duration
}

func (clock *cancelingStaleHostFenceClock) Now() time.Time { return clock.now }

func (clock *cancelingStaleHostFenceClock) Wait(ctx context.Context, delay time.Duration) error {
	clock.mu.Lock()
	clock.waits = append(clock.waits, delay)
	clock.mu.Unlock()
	if clock.waitCalls.Add(1) >= clock.cancelAt {
		clock.cancel()
		return context.Canceled
	}
	return nil
}

func (clock *cancelingStaleHostFenceClock) delays() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.waits...)
}

func equalDurations(left, right []time.Duration) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
