package dispatch

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func deterministicUUID(seed string) uuid.UUID {
	hash := sha256.Sum256([]byte(seed))
	var id uuid.UUID
	copy(id[:], hash[:])
	id[6] = id[6]&0x0f | 0x80
	id[8] = id[8]&0x3f | 0x80
	return id
}

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
