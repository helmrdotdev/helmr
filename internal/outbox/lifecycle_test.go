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
		pruned: []pgtype.UUID{pgvalue.UUID(uuid.NewV7())},
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
		!strings.Contains(got, `"pruned":1`) ||
		!strings.Contains(got, `"dead_lettered":3`) ||
		!strings.Contains(got, `"oldest_pending_age"`) {
		t.Fatalf("lifecycle log = %s", got)
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
	pruned []pgtype.UUID
	stats  db.ControlOutboxLifecycleRow
	prune  db.PruneDeliveredControlOutboxParams
}

func (s *lifecycleStore) PruneDeliveredControlOutbox(
	_ context.Context,
	params db.PruneDeliveredControlOutboxParams,
) ([]pgtype.UUID, error) {
	s.prune = params
	return s.pruned, nil
}

func (s *lifecycleStore) ControlOutboxLifecycle(context.Context) (db.ControlOutboxLifecycleRow, error) {
	return s.stats, nil
}
