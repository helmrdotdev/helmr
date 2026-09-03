package db_test

import (
	"context"
	"errors"
	"os"
	"strings"
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
	deadLetterID2 := uuid.NewV7()
	freshID := uuid.NewV7()
	oldA := uuid.NewV7()
	oldB := uuid.NewV7()
	oldC := uuid.NewV7()
	insertControlOutbox(t, ctx, pool, pendingID, "token.reconcile", "pending", time.Time{})
	insertControlOutbox(t, ctx, pool, claimedID, "token.reconcile", "claimed", time.Time{})
	insertControlOutbox(t, ctx, pool, deadLetterID, "token.reconcile", "dead_lettered", time.Time{})
	insertControlOutbox(t, ctx, pool, deadLetterID2, "token.reconcile", "dead_lettered", time.Time{})
	insertControlOutbox(t, ctx, pool, freshID, "token.reconcile", "delivered", time.Now().Add(-23*time.Hour))
	insertControlOutbox(t, ctx, pool, oldA, "token.reconcile", "delivered", time.Now().Add(-26*time.Hour))
	insertControlOutbox(t, ctx, pool, oldB, "secret.revoked", "delivered", time.Now().Add(-25*time.Hour))
	insertControlOutbox(t, ctx, pool, oldC, "session.input.reconcile", "delivered", time.Now().Add(-30*time.Hour))
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := queries.PruneDeliveredControlOutbox(canceledCtx, db.PruneDeliveredControlOutboxParams{
		RetainFor: pgvalue.Interval(outbox.DeliveredRetention),
		RowLimit:  2,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prune = %v", err)
	}
	if countControlOutbox(t, ctx, pool, oldA, oldB, oldC) != 3 {
		t.Fatal("canceled prune changed eligible rows")
	}

	first, err := queries.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
		RetainFor: pgvalue.Interval(outbox.DeliveredRetention),
		RowLimit:  2,
	})
	if err != nil || first != 2 {
		t.Fatalf("first prune = %v, %v", first, err)
	}
	assertControlOutboxPresent(t, ctx, pool, pendingID, claimedID, deadLetterID, deadLetterID2, freshID)
	remainingOld := countControlOutbox(t, ctx, pool, oldA, oldB, oldC)
	if remainingOld != 1 {
		t.Fatalf("old delivered remaining after first prune = %d, want 1", remainingOld)
	}

	second, err := queries.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
		RetainFor: pgvalue.Interval(outbox.DeliveredRetention),
		RowLimit:  2,
	})
	if err != nil || second != 1 {
		t.Fatalf("second prune = %v, %v", second, err)
	}
	assertControlOutboxPresent(t, ctx, pool, pendingID, claimedID, deadLetterID, deadLetterID2, freshID)
	if countControlOutbox(t, ctx, pool, oldA, oldB, oldC) != 0 {
		t.Fatal("repeated prune left delivered rows older than retention")
	}

	stats, err := queries.ControlOutboxLifecycle(ctx, 1)
	if err != nil || !stats.OldestEligibleAt.Valid || stats.DeadLetteredRows != 1 || !stats.DeadLetteredOverflow {
		t.Fatalf("lifecycle = %+v, %v", stats, err)
	}
}

func TestControlOutboxDeadLettersOnlyUnsupportedTopics(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)

	knownID := uuid.NewV7()
	unknownID := uuid.NewV7()
	insertControlOutbox(t, ctx, pool, knownID, "token.reconcile", "pending", time.Time{})
	insertControlOutbox(t, ctx, pool, unknownID, "unknown.topic", "pending", time.Time{})

	rows, err := queries.DeadLetterUnsupportedControlOutbox(ctx, db.DeadLetterUnsupportedControlOutboxParams{
		SupportedTopics: []string{
			"session.input.reconcile",
			"session.close.reconcile",
			"token.reconcile",
			"secret.revoked",
		},
		RowLimit: 100,
	})
	if err != nil || len(rows) != 1 || pgvalue.MustUUIDValue(rows[0].ID) != unknownID || rows[0].State != "dead_lettered" {
		t.Fatalf("dead-letter unsupported = %+v, %v", rows, err)
	}
	assertControlOutboxPresent(t, ctx, pool, knownID)
	var knownState string
	if err := pool.QueryRow(ctx, `SELECT state FROM control_outbox WHERE id = $1`, knownID).Scan(&knownState); err != nil {
		t.Fatal(err)
	}
	if knownState != "pending" {
		t.Fatalf("known topic state = %q, want pending", knownState)
	}
}

func TestControlOutboxDeadLettersUnsupportedAfterSupportedPrefixSaturation(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)

	const rowLimit int32 = 2
	supportedTopics := []string{
		"session.input.reconcile",
		"session.close.reconcile",
		"token.reconcile",
		"secret.revoked",
	}

	insertPendingAt := func(id uuid.UUID, topic string, createdAt time.Time) {
		dbtest.MustExec(t, ctx, pool, `
			INSERT INTO control_outbox (id, topic, payload, state, created_at)
			VALUES ($1, $2, '{}'::jsonb, 'pending', $3)
		`, id, topic, createdAt)
	}

	unsupportedStart := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	supportedIDs := []uuid.UUID{uuid.NewV7(), uuid.NewV7()}
	oldestUnsupportedID := uuid.NewV7()
	nextUnsupportedID := uuid.NewV7()
	newestUnsupportedID := uuid.NewV7()
	insertPendingAt(supportedIDs[0], "token.reconcile", unsupportedStart.Add(-2*time.Hour))
	insertPendingAt(supportedIDs[1], "token.reconcile", unsupportedStart.Add(-time.Hour))
	insertPendingAt(oldestUnsupportedID, "unknown.oldest", unsupportedStart)
	insertPendingAt(nextUnsupportedID, "unknown.next", unsupportedStart.Add(time.Hour))
	insertPendingAt(newestUnsupportedID, "unknown.newest", unsupportedStart.Add(2*time.Hour))

	rows, err := queries.DeadLetterUnsupportedControlOutbox(ctx, db.DeadLetterUnsupportedControlOutboxParams{
		SupportedTopics: supportedTopics,
		RowLimit:        rowLimit,
	})
	if err != nil {
		t.Fatalf("dead-letter unsupported after supported prefix = %+v, %v", rows, err)
	}
	if len(rows) != int(rowLimit) {
		t.Fatalf("dead-lettered rows = %d, want %d", len(rows), rowLimit)
	}
	deadLetteredIDs := make(map[uuid.UUID]bool, len(rows))
	for _, row := range rows {
		if row.State != "dead_lettered" {
			t.Fatalf("dead-lettered row state = %q, want dead_lettered", row.State)
		}
		deadLetteredIDs[pgvalue.MustUUIDValue(row.ID)] = true
	}
	for _, id := range []uuid.UUID{oldestUnsupportedID, nextUnsupportedID} {
		if !deadLetteredIDs[id] {
			t.Fatalf("oldest unsupported row %s was not dead-lettered", id)
		}
	}
	var pendingSupportedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::integer
		  FROM control_outbox
		 WHERE id = ANY($1::uuid[])
		   AND state = 'pending'
	`, pgUUIDs(supportedIDs)).Scan(&pendingSupportedCount); err != nil {
		t.Fatal(err)
	}
	if pendingSupportedCount != len(supportedIDs) {
		t.Fatalf("supported pending rows = %d, want %d", pendingSupportedCount, len(supportedIDs))
	}
	var newestUnsupportedState string
	if err := pool.QueryRow(ctx, `SELECT state FROM control_outbox WHERE id = $1`, newestUnsupportedID).Scan(&newestUnsupportedState); err != nil {
		t.Fatal(err)
	}
	if newestUnsupportedState != "pending" {
		t.Fatalf("newest unsupported topic state = %q, want pending", newestUnsupportedState)
	}
}

func TestControlOutboxLifecycleScaleBudget(t *testing.T) {
	if os.Getenv("HELMR_TEST_CONTROL_OUTBOX_SCALE") != "1" {
		t.Skip("HELMR_TEST_CONTROL_OUTBOX_SCALE is not set")
	}
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)

	start := time.Now()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO control_outbox (
			id, topic, payload, state, available_at, last_error, created_at, delivered_at
		)
		SELECT (substr(md5(generated::text), 1, 8) || '-' ||
		        substr(md5(generated::text), 9, 4) || '-' ||
		        substr(md5(generated::text), 13, 4) || '-' ||
		        substr(md5(generated::text), 17, 4) || '-' ||
		        substr(md5(generated::text), 21, 12))::uuid,
		       CASE WHEN generated > 800000 AND generated <= 900000
		            THEN 'unknown.topic' ELSE 'token.reconcile' END,
		       '{}'::jsonb,
		       CASE WHEN generated <= 700000 THEN 'delivered'
		            WHEN generated <= 900000 THEN 'pending'
		            ELSE 'dead_lettered' END,
		       CASE WHEN generated <= 600000 OR (generated > 700000 AND generated <= 900000)
		            THEN now() - interval '48 hours' ELSE now() END,
		       CASE WHEN generated > 900000 THEN 'fixture dead letter' END,
		       now() - ((1000000 - generated)::text || ' milliseconds')::interval,
		       CASE WHEN generated <= 600000 THEN now() - interval '48 hours'
		            WHEN generated <= 700000 THEN now() - interval '1 hour' END
		  FROM generate_series(1, 1000000) AS generated
	`)
	dbtest.MustExec(t, ctx, pool, `ANALYZE control_outbox`)
	t.Logf("seeded 1,000,000 control outbox rows in %s", time.Since(start))

	pruneTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prunePlan := controlOutboxExplain(t, ctx, pruneTx, `
		EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT TEXT)
		DELETE FROM control_outbox WHERE id IN (
			SELECT id FROM control_outbox
			 WHERE state = 'delivered'
			   AND delivered_at < now() - interval '24 hours'
			 ORDER BY delivered_at, id LIMIT 2500
		)
	`)
	if err := pruneTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prunePlan, "control_outbox_delivered_prune_idx") {
		t.Fatalf("prune plan missed delivered index:\n%s", prunePlan)
	}

	unsupportedTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unsupportedPlan := controlOutboxExplain(t, ctx, unsupportedTx, `
		EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
		WITH candidates AS MATERIALIZED (
			SELECT id FROM control_outbox
			 WHERE state = 'pending'
			   AND NOT (topic = ANY($1::text[]))
			 ORDER BY created_at, id LIMIT 2500
		)
		UPDATE control_outbox
		   SET state = 'dead_lettered', last_error = 'unsupported control outbox topic'
		  FROM candidates
		 WHERE control_outbox.id = candidates.id
		   AND control_outbox.state = 'pending'
	`, []string{"token.reconcile", "secret.revoked", "session.input.reconcile", "session.close.reconcile"})
	if err := unsupportedTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unsupportedPlan, "control_outbox_pending_created_idx") ||
		strings.Contains(unsupportedPlan, "external merge") {
		t.Fatalf("unsupported-topic plan missed bounded index shape:\n%s", unsupportedPlan)
	}

	lifecyclePlan := controlOutboxExplain(t, ctx, pool, `
		EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
		WITH dead_letter_sample AS (
			SELECT 1 FROM control_outbox
			 WHERE state = 'dead_lettered'
			 ORDER BY created_at, id LIMIT 1001
		), dead_letter_counts AS (
			SELECT count(*)::bigint AS rows FROM dead_letter_sample
		)
		SELECT (
			SELECT available_at FROM control_outbox
			 WHERE state = 'pending' AND available_at <= now()
			 ORDER BY available_at, id LIMIT 1
		), LEAST(rows, 1000), rows > 1000
		FROM dead_letter_counts
	`)
	if !strings.Contains(lifecyclePlan, "control_outbox_pending_available_idx") ||
		!strings.Contains(lifecyclePlan, "control_outbox_dead_lettered_created_idx") ||
		strings.Contains(lifecyclePlan, "Seq Scan on control_outbox") {
		t.Fatalf("lifecycle plan missed bounded indexes:\n%s", lifecyclePlan)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE control_outbox
		   SET available_at = now() + interval '48 hours'
		 WHERE state = 'pending'
	`)
	dbtest.MustExec(t, ctx, pool, `ANALYZE control_outbox`)
	futureOnlyPlan := controlOutboxExplain(t, ctx, pool, `
		EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
		SELECT available_at FROM control_outbox
		 WHERE state = 'pending' AND available_at <= now()
		 ORDER BY available_at, id LIMIT 1
	`)
	if !strings.Contains(futureOnlyPlan, "control_outbox_pending_available_idx") ||
		strings.Contains(futureOnlyPlan, "Seq Scan on control_outbox") {
		t.Fatalf("future-only eligibility plan missed available index:\n%s", futureOnlyPlan)
	}

	var tempFilesBefore, tempBytesBefore int64
	if err := pool.QueryRow(ctx, `
		SELECT temp_files, temp_bytes FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&tempFilesBefore, &tempBytesBefore); err != nil {
		t.Fatal(err)
	}
	var walBefore string
	if err := pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&walBefore); err != nil {
		t.Fatal(err)
	}
	tickStart := time.Now()
	var deleted int64
	for range 16 {
		rows, err := queries.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
			RetainFor: pgvalue.Interval(outbox.DeliveredRetention), RowLimit: 2500,
		})
		if err != nil {
			t.Fatal(err)
		}
		deleted += rows
	}
	tickDuration := time.Since(tickStart)
	var walBytes, tempFilesAfter, tempBytesAfter int64
	if err := pool.QueryRow(ctx, `
		SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint,
		       temp_files, temp_bytes
		  FROM pg_stat_database WHERE datname = current_database()
	`, walBefore).Scan(&walBytes, &tempFilesAfter, &tempBytesAfter); err != nil {
		t.Fatal(err)
	}
	t.Logf("control lifecycle tick: deleted=%d duration=%s wal=%d temp_files=%d temp_bytes=%d",
		deleted, tickDuration, walBytes, tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore)
	if deleted != 40000 {
		t.Fatalf("deleted = %d, want 40000", deleted)
	}
	if tickDuration > time.Second {
		t.Fatalf("tick duration = %s, budget <= 1s", tickDuration)
	}
	if walBytes > 32<<20 {
		t.Fatalf("tick WAL = %d, budget <= %d", walBytes, 32<<20)
	}
	if tempFilesAfter != tempFilesBefore || tempBytesAfter != tempBytesBefore {
		t.Fatalf("tick created temp files/bytes: files=%d bytes=%d",
			tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore)
	}
}

type controlOutboxPlanQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func controlOutboxExplain(
	t *testing.T,
	ctx context.Context,
	q controlOutboxPlanQuerier,
	query string,
	args ...any,
) string {
	t.Helper()
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
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
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::integer
		  FROM control_outbox
		 WHERE id = ANY($1::uuid[])
	`, pgUUIDs(ids)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	values := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		values[i] = pgvalue.UUID(id)
	}
	return values
}
