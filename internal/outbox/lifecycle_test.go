package outbox

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestLifecycleLogsPendingAgeAndDeadLetterCount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := &lifecycleStore{
		pruned: []int64{
			int64(lifecycleBatchSize),
			1,
		},
		stats: db.ControlOutboxLifecycleRow{
			OldestEligibleAt:     pgvalue.Timestamptz(now.Add(-90 * time.Second)),
			DeadLetteredRows:     3,
			DeadLetteredOverflow: true,
		},
	}
	var logs bytes.Buffer
	loop, err := NewLifecycle(slog.New(slog.NewJSONHandler(&logs, nil)), store)
	if err != nil {
		t.Fatal(err)
	}
	loop.now = func() time.Time { return now }
	if err := loop.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.prune.RowLimit != lifecycleBatchSize || store.prune.RetainFor != pgvalue.Interval(DeliveredRetention) {
		t.Fatalf("prune params = %+v", store.prune)
	}
	if store.lifecycleLimit != deadLetterObservationLimit {
		t.Fatalf("lifecycle limit = %d", store.lifecycleLimit)
	}
	got := logs.String()
	if !strings.Contains(got, `"msg":"control outbox lifecycle"`) ||
		!strings.Contains(got, `"pruned":2501`) ||
		!strings.Contains(got, `"dead_lettered_rows":3`) ||
		!strings.Contains(got, `"dead_lettered_overflow":true`) ||
		!strings.Contains(got, `"oldest_eligible_age"`) {
		t.Fatalf("lifecycle log = %s", got)
	}
}

func TestLifecycleDeadLettersAndWarnsForUnsupportedTopic(t *testing.T) {
	id := uuid.NewV7()
	store := &lifecycleStore{unsupported: []db.ControlOutbox{{
		ID: pgvalue.UUID(id), Topic: "unknown.topic",
	}}}
	var logs bytes.Buffer
	loop, err := NewLifecycle(slog.New(slog.NewJSONHandler(&logs, nil)), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.unsupportedParams.SupportedTopics) != len(supportedTopics) {
		t.Fatalf("supported topics = %v", store.unsupportedParams.SupportedTopics)
	}
	got := logs.String()
	if !strings.Contains(got, `"msg":"control outbox dead-lettered"`) ||
		!strings.Contains(got, `"id":"`+id.String()+`"`) ||
		!strings.Contains(got, `"topic":"unknown.topic"`) {
		t.Fatalf("dead-letter log = %s", got)
	}
}

func TestLifecycleDoesNotLogAnEmptyTick(t *testing.T) {
	var logs bytes.Buffer
	loop, err := NewLifecycle(
		slog.New(slog.NewJSONHandler(&logs, nil)),
		&lifecycleStore{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("empty lifecycle log = %s", logs.String())
	}
}

func TestLifecyclePruneHasFixedPerTickBudget(t *testing.T) {
	pruned := make([]int64, lifecyclePruneBatchCount+1)
	for i := range pruned {
		pruned[i] = int64(lifecycleBatchSize)
	}
	store := &lifecycleStore{pruned: pruned}
	loop, err := NewLifecycle(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.pruneCalls != lifecyclePruneBatchCount {
		t.Fatalf("prune calls = %d, want %d", store.pruneCalls, lifecyclePruneBatchCount)
	}
}

func TestLifecyclePruneResumesAfterCancellation(t *testing.T) {
	store := &lifecycleStore{
		pruned:       []int64{int64(lifecycleBatchSize), 1},
		pruneErrCall: 1,
		pruneErr:     context.Canceled,
	}
	loop, err := NewLifecycle(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.tick(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled tick = %v", err)
	}
	store.pruneErr = nil
	if err := loop.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.pruneCalls != 3 {
		t.Fatalf("prune calls after resume = %d, want 3", store.pruneCalls)
	}
}

type lifecycleStore struct {
	unsupported       []db.ControlOutbox
	unsupportedParams db.DeadLetterUnsupportedControlOutboxParams
	pruned            []int64
	stats             db.ControlOutboxLifecycleRow
	prune             db.PruneDeliveredControlOutboxParams
	pruneCalls        int
	pruneIndex        int
	pruneErrCall      int
	pruneErr          error
	lifecycleLimit    int64
}

func (s *lifecycleStore) DeadLetterUnsupportedControlOutbox(
	_ context.Context,
	params db.DeadLetterUnsupportedControlOutboxParams,
) ([]db.ControlOutbox, error) {
	s.unsupportedParams = params
	return s.unsupported, nil
}

func (s *lifecycleStore) PruneDeliveredControlOutbox(
	_ context.Context,
	params db.PruneDeliveredControlOutboxParams,
) (int64, error) {
	s.prune = params
	call := s.pruneCalls
	s.pruneCalls++
	if s.pruneErr != nil && call == s.pruneErrCall {
		return 0, s.pruneErr
	}
	if s.pruneIndex >= len(s.pruned) {
		return 0, nil
	}
	pruned := s.pruned[s.pruneIndex]
	s.pruneIndex++
	return pruned, nil
}

func (s *lifecycleStore) ControlOutboxLifecycle(_ context.Context, limit int64) (db.ControlOutboxLifecycleRow, error) {
	s.lifecycleLimit = limit
	return s.stats, nil
}
