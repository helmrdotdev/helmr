package db_test

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func placeEnvironmentInOtherWorkerGroup(t testingT, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) string {
	t.Helper()
	otherWorkerGroupID := dbtest.DefaultWorkerGroupID + "-other"
	ensureWorkerGroupPlacement(t, ctx, pool, ids, otherWorkerGroupID)
	return otherWorkerGroupID
}

func disableDefaultWorkerGroupPlacement(t testingT, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET state = 'draining'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}
}

func ensureWorkerGroupPlacement(t testingT, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, workerGroupID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_groups (id, region_id, name, state, health_state, routing_fresh_until)
		VALUES ($1, $2, $3, 'active', 'healthy', now() + interval '5 minutes')
		ON CONFLICT (id) DO UPDATE
		   SET state = 'active',
		       health_state = 'healthy',
		       routing_fresh_until = now() + interval '5 minutes'
	`, workerGroupID, dbtest.DefaultRegionID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if workerGroupID != dbtest.DefaultWorkerGroupID {
		disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)
	}
}

type testingT interface {
	Helper()
	Fatal(args ...any)
}
