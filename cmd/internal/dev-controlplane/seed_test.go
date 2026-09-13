package main

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
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
	freshInit, err := migrate(ctx, pool)
	if err != nil {
		t.Fatalf("migrate fresh database: %v", err)
	}
	if !freshInit {
		t.Fatal("expected fresh database initialization")
	}
	q := db.New(pool)
	if _, err := q.CreateRegion(ctx, db.CreateRegionParams{ID: "dev-local", DisplayName: "Local"}); err != nil {
		t.Fatalf("bootstrap local region: %v", err)
	}
	if _, err := q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID: pgvalue.NewUUIDv7(), TokenID: pgvalue.NewUUIDv7(), TokenHash: make([]byte, 32),
		RegionID: "dev-local", Name: "default",
	}); err != nil {
		t.Fatalf("bootstrap local worker group: %v", err)
	}
	cfg := devConfig{bootstrap: config.Bootstrap{RegionID: "dev-local"}}
	if err := seedDevData(ctx, pool, cfg); err != nil {
		t.Fatalf("seed fresh database: %v", err)
	}

	var projects, environments, productionDeployments, demoDeployments int
	if err := pool.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM projects WHERE org_id = '00000000-0000-7000-8000-000000000201'),
		    (SELECT count(*) FROM environments WHERE org_id = '00000000-0000-7000-8000-000000000201'),
		    (SELECT count(*) FROM deployments WHERE environment_id = '00000000-0000-7000-8000-000000000401'),
		    (SELECT count(*) FROM deployments WHERE environment_id = '00000000-0000-7000-8000-000000000403')
	`).Scan(&projects, &environments, &productionDeployments, &demoDeployments); err != nil {
		t.Fatal(err)
	}
	if projects != 1 || environments != 3 || productionDeployments != 0 || demoDeployments != 1 {
		t.Fatalf(
			"seeded projects/environments/production deployments/demo deployments = %d/%d/%d/%d, want 1/3/0/1",
			projects,
			environments,
			productionDeployments,
			demoDeployments,
		)
	}
}

func TestDevSeedPreservesUserEditsAcrossRestart(t *testing.T) {
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
	freshInit, err := migrate(ctx, pool)
	if err != nil {
		t.Fatalf("migrate fresh database: %v", err)
	}
	if !freshInit {
		t.Fatal("expected fresh database initialization")
	}
	q := db.New(pool)
	if _, err := q.CreateRegion(ctx, db.CreateRegionParams{ID: "dev-local", DisplayName: "Local"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID: pgvalue.NewUUIDv7(), TokenID: pgvalue.NewUUIDv7(), TokenHash: make([]byte, 32),
		RegionID: "dev-local", Name: "default",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := devConfig{bootstrap: config.Bootstrap{RegionID: "dev-local"}}
	if err := seedDevData(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE environments
		   SET name = 'Renamed Production', color_hex = '#22C55E'
		 WHERE id = '00000000-0000-7000-8000-000000000401'
	`); err != nil {
		t.Fatal(err)
	}
	freshInit, err = migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if freshInit {
		t.Fatal("expected existing database on second migrate")
	}
	var name, color string
	if err := pool.QueryRow(ctx, `
		SELECT name, color_hex FROM environments WHERE id = '00000000-0000-7000-8000-000000000401'
	`).Scan(&name, &color); err != nil {
		t.Fatal(err)
	}
	if name != "Renamed Production" || color != "#22C55E" {
		t.Fatalf("environment after restart = %q/%q, want Renamed Production/#22C55E", name, color)
	}
}

func TestMigrateRejectsSchemaVersionMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := dbtest.Open(t).Pool
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping schema version test", serverVersion)
	}
	freshInit, err := migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !freshInit {
		t.Fatal("expected fresh database initialization")
	}
	want, err := schema.CurrentVersion()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET version = $1`, want+1); err != nil {
		t.Fatal(err)
	}
	if err := verifySchemaVersion(ctx, pool); err == nil {
		t.Fatal("expected schema version mismatch error")
	}
}
