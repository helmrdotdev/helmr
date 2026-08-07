package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDevSeedWithFreshPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := dbtest.Open(t).Pool
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping dev seed integration test", serverVersion)
	}
	if err := migrate(ctx, pool, false); err != nil {
		t.Fatalf("migrate fresh database: %v", err)
	}
	q := db.New(pool)
	if _, err := q.CreateRegion(ctx, db.CreateRegionParams{
		ID: "dev-local", Provider: "local", ProviderRegion: "local", DisplayName: "Local",
		State: db.RegionStateAvailable,
	}); err != nil {
		t.Fatalf("bootstrap local region: %v", err)
	}
	if _, err := q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID: uuid.Must(uuid.NewV7()).String(), TokenID: pgvalue.NewUUIDv7(), TokenHash: make([]byte, 32),
		RegionID: "dev-local", Name: "default", AllowsRun: true, AllowsBuild: true,
		RequiredCPUMillis: 1, RequiredMemoryBytes: 1, RequiredGuestEphemeralDiskBytes: 1,
		RequiredVMSlots: 1, RequiredBuildExecutors: 1, ObservationTtlSeconds: 120,
	}); err != nil {
		t.Fatalf("bootstrap local worker group: %v", err)
	}
	cfg := devConfig{bootstrapRegionID: "dev-local"}
	if err := seedDevData(ctx, pool, cfg); err != nil {
		t.Fatalf("seed fresh database: %v", err)
	}
	if err := seedDevData(ctx, pool, cfg); err != nil {
		t.Fatalf("seed should be idempotent: %v", err)
	}

	var projects, environments, deployments int
	if err := pool.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM projects WHERE org_id = '00000000-0000-7000-8000-000000000201'),
		    (SELECT count(*) FROM environments WHERE org_id = '00000000-0000-7000-8000-000000000201'),
		    (SELECT count(*) FROM deployments WHERE org_id = '00000000-0000-7000-8000-000000000201')
	`).Scan(&projects, &environments, &deployments); err != nil {
		t.Fatal(err)
	}
	if projects != 1 || environments != 2 || deployments != 0 {
		t.Fatalf(
			"seeded projects/environments/deployments = %d/%d/%d, want 1/2/0",
			projects,
			environments,
			deployments,
		)
	}
}
