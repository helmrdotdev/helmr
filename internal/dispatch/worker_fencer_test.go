package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestStaleWorkerFencerFencesStaleActiveWorker(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleWorkerCandidate(1, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleWorkerFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate}}
	fencer := newTestStaleWorkerFencer(t, store, now)

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
	if got := store.rechecks[0].ReasonCode.String; got != staleWorkerReasonCode {
		t.Fatalf("reason code = %q, want %q", got, staleWorkerReasonCode)
	}
}

func TestStaleWorkerFencerExcludesFreshDisabledAndLostWorkers(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	store := &fakeStaleWorkerFenceQueries{}
	fencer := newTestStaleWorkerFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Selected != 0 || cycle.Fenced != 0 || len(store.rechecks) != 0 {
		t.Fatalf("cycle = %+v rechecks = %+v, want no eligible workers", cycle, store.rechecks)
	}
	if got := store.listParams[0].ObservationStaleBefore.Time; !got.Equal(now.Add(-DefaultStaleWorkerGrace)) {
		t.Fatalf("observation stale cutoff = %v, want %v", got, now.Add(-DefaultStaleWorkerGrace))
	}
	if got := store.listParams[0].RegistrationStaleBefore.Time; !got.Equal(now.Add(-DefaultWorkerRegistrationReadinessGrace)) {
		t.Fatalf("registration stale cutoff = %v, want %v", got, now.Add(-DefaultWorkerRegistrationReadinessGrace))
	}
}

func TestStaleWorkerFencerLateFreshObservationWinsRecheck(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleWorkerCandidate(2, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleWorkerFenceQueries{
		candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate},
		recheck: func(context.Context, db.RecheckAndFenceStaleWorkerInstanceParams) (db.RecheckAndFenceStaleWorkerInstanceRow, error) {
			return db.RecheckAndFenceStaleWorkerInstanceRow{}, pgx.ErrNoRows
		},
	}
	fencer := newTestStaleWorkerFencer(t, store, now)

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

func TestStaleWorkerFencerOldEpochCannotFenceNewEpoch(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleWorkerCandidate(3, db.WorkerInstanceStateDraining, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleWorkerFenceQueries{
		candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate},
		recheck: func(_ context.Context, params db.RecheckAndFenceStaleWorkerInstanceParams) (db.RecheckAndFenceStaleWorkerInstanceRow, error) {
			if params.ExpectedEpoch.Int64 != 7 {
				t.Fatalf("expected epoch = %d, want selected epoch 7", params.ExpectedEpoch.Int64)
			}
			return db.RecheckAndFenceStaleWorkerInstanceRow{}, pgx.ErrNoRows
		},
	}
	fencer := newTestStaleWorkerFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Fenced != 0 || cycle.Skipped != 1 {
		t.Fatalf("cycle = %+v, want changed epoch skipped", cycle)
	}
}

func TestStaleWorkerFencerHandlesRegisteringWorkerWithoutObservation(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := staleWorkerCandidate(4, db.WorkerInstanceStateRegistering, now.Add(-time.Minute), "registering_observation_missing")
	store := &fakeStaleWorkerFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{candidate}}
	fencer := newTestStaleWorkerFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Fenced != 1 || cycle.Results[0].Reason != "registering_observation_missing" {
		t.Fatalf("cycle = %+v, want missing registering observation fenced", cycle)
	}
}

func TestStaleWorkerFencerIsDeploymentModeAgnostic(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	managed := staleWorkerCandidate(5, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	managed.WorkerGroupID = "managed-run"
	selfHosted := staleWorkerCandidate(6, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	selfHosted.WorkerGroupID = "self-hosted-run"
	store := &fakeStaleWorkerFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{managed, selfHosted}}
	fencer := newTestStaleWorkerFencer(t, store, now)

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

func TestStaleWorkerFencerUsesWorkerGroupCutoffs(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	run := staleWorkerCandidate(7, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	run.WorkerGroupID = "run"
	build := staleWorkerCandidate(8, db.WorkerInstanceStateRegistering, now.Add(-time.Minute), "registering_observation_missing")
	build.WorkerGroupID = "build"
	store := &fakeStaleWorkerFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{run, build}}
	fencer, err := NewStaleWorkerFencer(
		fakeStaleWorkerFenceTransactions{queries: store},
		WithStaleWorkerFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleWorkerFenceClock(fixedStaleWorkerFenceClock{now: now}),
		WithWorkerGroupFenceGrace(map[string]WorkerGroupFenceGrace{
			"run":   {Observation: 30 * time.Second, Registration: time.Minute},
			"build": {Observation: 5 * time.Minute, Registration: 20 * time.Minute},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fencer.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.listParams) != 2 || store.listParams[0].WorkerGroupID != "build" || store.listParams[1].WorkerGroupID != "run" {
		t.Fatalf("list scopes = %+v", store.listParams)
	}
	if got, want := store.listParams[1].ObservationStaleBefore.Time, now.Add(-30*time.Second); !got.Equal(want) {
		t.Fatalf("run selection observation cutoff = %v, want %v", got, want)
	}
	if got, want := store.rechecks[1].ObservationStaleBefore.Time, now.Add(-30*time.Second); !got.Equal(want) {
		t.Fatalf("run observation cutoff = %v, want %v", got, want)
	}
	if got, want := store.rechecks[0].RegistrationStaleBefore.Time, now.Add(-20*time.Minute); !got.Equal(want) {
		t.Fatalf("build registration cutoff = %v, want %v", got, want)
	}
}

func TestStaleWorkerFencerScopesBatchLimitPerGroup(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	candidates := make([]db.ListStaleWorkerFenceCandidatesRow, 0, 102)
	for index := range 101 {
		candidate := staleWorkerCandidate(byte(index+1), db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
		candidate.WorkerGroupID = "build"
		candidates = append(candidates, candidate)
	}
	run := staleWorkerCandidate(255, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	run.WorkerGroupID = "run"
	candidates = append(candidates, run)
	store := &fakeStaleWorkerFenceQueries{candidates: candidates}
	store.recheck = func(_ context.Context, params db.RecheckAndFenceStaleWorkerInstanceParams) (db.RecheckAndFenceStaleWorkerInstanceRow, error) {
		if params.WorkerGroupID == "build" {
			return db.RecheckAndFenceStaleWorkerInstanceRow{}, pgx.ErrNoRows
		}
		return db.RecheckAndFenceStaleWorkerInstanceRow{ID: params.ID, WorkerGroupID: params.WorkerGroupID}, nil
	}
	fencer, err := NewStaleWorkerFencer(
		fakeStaleWorkerFenceTransactions{queries: store},
		WithStaleWorkerFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleWorkerFenceClock(fixedStaleWorkerFenceClock{now: now}),
		WithWorkerGroupFenceGrace(map[string]WorkerGroupFenceGrace{
			"run":   {Observation: 30 * time.Second, Registration: time.Minute},
			"build": {Observation: 5 * time.Minute, Registration: 20 * time.Minute},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := fencer.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Fenced != 1 || store.rechecks[len(store.rechecks)-1].WorkerGroupID != "run" {
		t.Fatalf("cycle=%+v last recheck=%+v", cycle, store.rechecks[len(store.rechecks)-1])
	}
}

func TestStaleWorkerFencerPersistentFailureRetriesUntilCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := &cancelingStaleWorkerFenceClock{
		now:       time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		cancel:    cancel,
		cancelAt:  4,
		waitCalls: &atomic.Int32{},
	}
	transactions := &failingStaleWorkerFenceTransactions{err: errors.New("database unavailable")}
	fencer, err := NewStaleWorkerFencer(
		transactions,
		WithStaleWorkerFenceInterval(time.Millisecond),
		WithStaleWorkerFenceTimeout(time.Second),
		WithStaleWorkerFenceMaxBackoff(8*time.Millisecond),
		WithStaleWorkerFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleWorkerFenceClock(clock),
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

func TestStaleWorkerFenceFailureRollsBackReportedResults(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	first := staleWorkerCandidate(7, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	second := staleWorkerCandidate(8, db.WorkerInstanceStateActive, now.Add(-time.Minute), "worker_observation_stale")
	store := &fakeStaleWorkerFenceQueries{
		candidates: []db.ListStaleWorkerFenceCandidatesRow{first, second},
		recheck: func(_ context.Context, params db.RecheckAndFenceStaleWorkerInstanceParams) (db.RecheckAndFenceStaleWorkerInstanceRow, error) {
			if params.ID == second.ID {
				return db.RecheckAndFenceStaleWorkerInstanceRow{}, errors.New("write failed")
			}
			return db.RecheckAndFenceStaleWorkerInstanceRow{ID: params.ID}, nil
		},
	}
	fencer := newTestStaleWorkerFencer(t, store, now)

	cycle, err := fencer.ReconcileOnce(context.Background())
	if err == nil {
		t.Fatal("ReconcileOnce unexpectedly succeeded")
	}
	if cycle.Selected != 0 || cycle.Fenced != 0 || len(cycle.Results) != 0 {
		t.Fatalf("cycle = %+v, must not report rolled-back fences", cycle)
	}
}

func newTestStaleWorkerFencer(t *testing.T, store *fakeStaleWorkerFenceQueries, now time.Time) *StaleWorkerFencer {
	t.Helper()
	fencer, err := NewStaleWorkerFencer(
		fakeStaleWorkerFenceTransactions{queries: store},
		WithStaleWorkerFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStaleWorkerFenceClock(fixedStaleWorkerFenceClock{now: now}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return fencer
}

func staleWorkerCandidate(seed byte, state db.WorkerInstanceState, freshness time.Time, reason string) db.ListStaleWorkerFenceCandidatesRow {
	var id [16]byte
	id[15] = seed
	return db.ListStaleWorkerFenceCandidatesRow{
		ID:            pgtype.UUID{Bytes: id, Valid: true},
		WorkerGroupID: "run",
		CurrentEpoch:  pgtype.Int8{Int64: 7, Valid: true},
		State:         state,
		FreshnessAt:   pgtype.Timestamptz{Time: freshness, Valid: true},
		Reason:        reason,
	}
}

type fakeStaleWorkerFenceTransactions struct {
	queries StaleWorkerFenceQueries
}

func (transactions fakeStaleWorkerFenceTransactions) WithinStaleWorkerFenceTransaction(
	_ context.Context,
	fn func(StaleWorkerFenceQueries) error,
) error {
	return fn(transactions.queries)
}

type fakeStaleWorkerFenceQueries struct {
	candidates []db.ListStaleWorkerFenceCandidatesRow
	listParams []db.ListStaleWorkerFenceCandidatesParams
	rechecks   []db.RecheckAndFenceStaleWorkerInstanceParams
	recheck    func(context.Context, db.RecheckAndFenceStaleWorkerInstanceParams) (db.RecheckAndFenceStaleWorkerInstanceRow, error)
}

func (queries *fakeStaleWorkerFenceQueries) ListStaleWorkerFenceCandidates(
	_ context.Context,
	params db.ListStaleWorkerFenceCandidatesParams,
) ([]db.ListStaleWorkerFenceCandidatesRow, error) {
	queries.listParams = append(queries.listParams, params)
	candidates := make([]db.ListStaleWorkerFenceCandidatesRow, 0, len(queries.candidates))
	for _, candidate := range queries.candidates {
		if params.WorkerGroupID == "" || candidate.WorkerGroupID == params.WorkerGroupID {
			candidates = append(candidates, candidate)
		}
		if len(candidates) == int(params.RowLimit) {
			break
		}
	}
	return candidates, nil
}

func (queries *fakeStaleWorkerFenceQueries) RecheckAndFenceStaleWorkerInstance(
	ctx context.Context,
	params db.RecheckAndFenceStaleWorkerInstanceParams,
) (db.RecheckAndFenceStaleWorkerInstanceRow, error) {
	queries.rechecks = append(queries.rechecks, params)
	if queries.recheck != nil {
		return queries.recheck(ctx, params)
	}
	return db.RecheckAndFenceStaleWorkerInstanceRow{
		ID: params.ID, WorkerGroupID: params.WorkerGroupID, CurrentEpoch: params.ExpectedEpoch,
	}, nil
}

type failingStaleWorkerFenceTransactions struct {
	err   error
	calls atomic.Int32
}

func (transactions *failingStaleWorkerFenceTransactions) WithinStaleWorkerFenceTransaction(
	context.Context,
	func(StaleWorkerFenceQueries) error,
) error {
	transactions.calls.Add(1)
	return transactions.err
}

type fixedStaleWorkerFenceClock struct {
	now time.Time
}

func (clock fixedStaleWorkerFenceClock) Now() time.Time { return clock.now }

func (fixedStaleWorkerFenceClock) Wait(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

type cancelingStaleWorkerFenceClock struct {
	now       time.Time
	cancel    context.CancelFunc
	cancelAt  int32
	waitCalls *atomic.Int32
	mu        sync.Mutex
	waits     []time.Duration
}

func (clock *cancelingStaleWorkerFenceClock) Now() time.Time { return clock.now }

func (clock *cancelingStaleWorkerFenceClock) Wait(ctx context.Context, delay time.Duration) error {
	clock.mu.Lock()
	clock.waits = append(clock.waits, delay)
	clock.mu.Unlock()
	if clock.waitCalls.Add(1) >= clock.cancelAt {
		clock.cancel()
		return context.Canceled
	}
	return nil
}

func (clock *cancelingStaleWorkerFenceClock) delays() []time.Duration {
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
