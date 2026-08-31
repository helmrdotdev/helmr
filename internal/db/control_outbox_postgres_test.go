package db_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/outbox"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestControlOutboxClaimReclaimAndStaleFence(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)
	id := uuid.NewV7()
	insertControlOutbox(t, ctx, pool, id, "token.reconcile", "pending", time.Time{})

	first, err := queries.ClaimControlOutbox(ctx, db.ClaimControlOutboxParams{
		ClaimedBy:      pgvalue.Text("worker-a"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Topics:         []string{"token.reconcile"},
		RowLimit:       8,
	})
	if err != nil || len(first) != 1 || pgvalue.MustUUIDValue(first[0].ID) != id ||
		first[0].Attempts != 1 || first[0].State != "claimed" {
		t.Fatalf("first claim = %+v, %v", first, err)
	}

	if _, err := queries.DeliverControlOutbox(ctx, db.DeliverControlOutboxParams{
		ID: pgvalue.UUID(id), ClaimedBy: pgvalue.Text("worker-b"), ClaimAttempt: first[0].Attempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign deliver = %v, want no rows", err)
	}
	if _, err := queries.DeliverControlOutbox(ctx, db.DeliverControlOutboxParams{
		ID: pgvalue.UUID(id), ClaimedBy: pgvalue.Text("worker-a"), ClaimAttempt: first[0].Attempts - 1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale attempt deliver = %v, want no rows", err)
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE control_outbox
		   SET claim_expires_at = now() - interval '1 second'
		 WHERE id = $1
	`, id)

	second, err := queries.ClaimControlOutbox(ctx, db.ClaimControlOutboxParams{
		ClaimedBy:      pgvalue.Text("worker-b"),
		ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Topics:         []string{"token.reconcile"},
		RowLimit:       8,
	})
	if err != nil || len(second) != 1 || second[0].Attempts != 2 ||
		pgvalue.TextValue(second[0].ClaimedBy) != "worker-b" {
		t.Fatalf("reclaim = %+v, %v", second, err)
	}

	if _, err := queries.DeliverControlOutbox(ctx, db.DeliverControlOutboxParams{
		ID: pgvalue.UUID(id), ClaimedBy: pgvalue.Text("worker-a"), ClaimAttempt: first[0].Attempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale reclaim deliver = %v, want no rows", err)
	}
	delivered, err := queries.DeliverControlOutbox(ctx, db.DeliverControlOutboxParams{
		ID: pgvalue.UUID(id), ClaimedBy: pgvalue.Text("worker-b"), ClaimAttempt: second[0].Attempts,
	})
	if err != nil || delivered.State != "delivered" {
		t.Fatalf("fenced deliver = %+v, %v", delivered, err)
	}
}

func TestControlOutboxPruneIsBoundedAndProtectsLiveStates(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)

	pendingID := uuid.NewV7()
	claimedID := uuid.NewV7()
	deadLetterID := uuid.NewV7()
	freshID := uuid.NewV7()
	oldA := uuid.NewV7()
	oldB := uuid.NewV7()
	oldC := uuid.NewV7()
	insertControlOutbox(t, ctx, pool, pendingID, "token.reconcile", "pending", time.Time{})
	insertControlOutbox(t, ctx, pool, claimedID, "token.reconcile", "claimed", time.Time{})
	insertControlOutbox(t, ctx, pool, deadLetterID, "token.reconcile", "dead_lettered", time.Time{})
	insertControlOutbox(t, ctx, pool, freshID, "token.reconcile", "delivered", time.Now().Add(-23*time.Hour))
	insertControlOutbox(t, ctx, pool, oldA, "token.reconcile", "delivered", time.Now().Add(-26*time.Hour))
	insertControlOutbox(t, ctx, pool, oldB, "secret.revoked", "delivered", time.Now().Add(-25*time.Hour))
	insertControlOutbox(t, ctx, pool, oldC, "session.input.reconcile", "delivered", time.Now().Add(-30*time.Hour))

	first, err := queries.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
		RetainFor: pgvalue.Interval(outbox.DeliveredRetention),
		RowLimit:  2,
	})
	if err != nil || len(first) != 2 {
		t.Fatalf("first prune = %v, %v", first, err)
	}
	assertControlOutboxPresent(t, ctx, pool, pendingID, claimedID, deadLetterID, freshID)
	remainingOld := countControlOutbox(t, ctx, pool, oldA, oldB, oldC)
	if remainingOld != 1 {
		t.Fatalf("old delivered remaining after first prune = %d, want 1", remainingOld)
	}

	second, err := queries.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
		RetainFor: pgvalue.Interval(outbox.DeliveredRetention),
		RowLimit:  2,
	})
	if err != nil || len(second) != 1 {
		t.Fatalf("second prune = %v, %v", second, err)
	}
	assertControlOutboxPresent(t, ctx, pool, pendingID, claimedID, deadLetterID, freshID)
	if countControlOutbox(t, ctx, pool, oldA, oldB, oldC) != 0 {
		t.Fatal("repeated prune left delivered rows older than retention")
	}

	stats, err := queries.ControlOutboxLifecycle(ctx)
	if err != nil || !stats.OldestPendingAt.Valid || stats.DeadLettered != 1 {
		t.Fatalf("lifecycle = %+v, %v", stats, err)
	}
}

func insertControlOutbox(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	id uuid.UUID,
	topic string,
	state string,
	deliveredAt time.Time,
) {
	t.Helper()
	switch state {
	case "pending":
		dbtest.MustExec(t, ctx, pool, `
			INSERT INTO control_outbox (id, topic, payload, state)
			VALUES ($1, $2, '{}'::jsonb, 'pending')
		`, id, topic)
	case "claimed":
		dbtest.MustExec(t, ctx, pool, `
			INSERT INTO control_outbox (
			    id, topic, payload, state, claimed_by, claim_expires_at, attempts
			) VALUES ($1, $2, '{}'::jsonb, 'claimed', 'worker-a', now() + interval '1 minute', 1)
		`, id, topic)
	case "delivered":
		dbtest.MustExec(t, ctx, pool, `
			INSERT INTO control_outbox (
			    id, topic, payload, state, delivered_at, available_at, created_at
			) VALUES ($1, $2, '{}'::jsonb, 'delivered', $3, $3, $3)
		`, id, topic, deliveredAt)
	case "dead_lettered":
		dbtest.MustExec(t, ctx, pool, `
			INSERT INTO control_outbox (
			    id, topic, payload, state, last_error
			) VALUES ($1, $2, '{}'::jsonb, 'dead_lettered', 'malformed payload')
		`, id, topic)
	default:
		t.Fatalf("unknown control outbox state %q", state)
	}
}

func assertControlOutboxPresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	if countControlOutbox(t, ctx, pool, ids...) != len(ids) {
		t.Fatalf("protected control_outbox rows missing: want %d", len(ids))
	}
}

func countControlOutbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids ...uuid.UUID) int {
	t.Helper()
	values := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		values[i] = pgvalue.UUID(id)
	}
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::integer
		  FROM control_outbox
		 WHERE id = ANY($1::uuid[])
	`, values).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
