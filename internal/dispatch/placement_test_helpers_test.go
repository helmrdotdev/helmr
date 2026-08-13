package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newDispatchIntegrationDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	return database.Pool
}

func placementTestUUID(last byte) pgtype.UUID {
	value := pgtype.UUID{Valid: true}
	value.Bytes[15] = last
	return value
}

func testRunPlacementPolicy(limit int32) runPlacementPolicy {
	return runPlacementPolicy{
		idleInterval: time.Second, failureBackoff: time.Second, timeout: time.Second,
		workers: 1, organizationLimit: limit, attemptLimit: limit, parallelism: 1,
	}
}
