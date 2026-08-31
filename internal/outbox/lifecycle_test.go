package outbox

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLifecycleLogsPendingAgeAndDeadLetterCount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := &lifecycleStore{
		pruned: [][]pgtype.UUID{
			makeUUIDs(pruneBatchSize),
			{pgvalue.UUID(uuid.NewV7())},
		},
		stats: db.ControlOutboxLifecycleRow{
			OldestPendingAt: pgvalue.Timestamptz(now.Add(-90 * time.Second)),
			DeadLettered:    3,
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
	if store.prune.RowLimit != pruneBatchSize || store.prune.RetainFor != pgvalue.Interval(DeliveredRetention) {
		t.Fatalf("prune params = %+v", store.prune)
	}
	got := logs.String()
	if !strings.Contains(got, `"msg":"control outbox lifecycle"`) ||
		!strings.Contains(got, `"pruned":101`) ||
		!strings.Contains(got, `"dead_lettered":3`) ||
		!strings.Contains(got, `"oldest_pending_age"`) {
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

type lifecycleStore struct {
	unsupported       []db.ControlOutbox
	unsupportedParams db.DeadLetterUnsupportedControlOutboxParams
	pruned            [][]pgtype.UUID
	stats             db.ControlOutboxLifecycleRow
	prune             db.PruneDeliveredControlOutboxParams
	pruneCalls        int
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
) ([]pgtype.UUID, error) {
	s.prune = params
	if s.pruneCalls >= len(s.pruned) {
		return nil, nil
	}
	batch := s.pruned[s.pruneCalls]
	s.pruneCalls++
	return batch, nil
}

func (s *lifecycleStore) ControlOutboxLifecycle(context.Context) (db.ControlOutboxLifecycleRow, error) {
	return s.stats, nil
}

func makeUUIDs(count int32) []pgtype.UUID {
	ids := make([]pgtype.UUID, count)
	for i := range ids {
		ids[i] = pgvalue.UUID(uuid.NewV7())
	}
	return ids
}
