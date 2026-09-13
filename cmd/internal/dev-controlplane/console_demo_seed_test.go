package main

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDemoEnvironmentSeedWithFreshPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := dbtest.Open(t).Pool
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping demo environment seed integration test", serverVersion)
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
		t.Fatalf("seed dev data: %v", err)
	}

	var (
		definitions   int
		schedules     int
		workspaces    int
		sessions      int
		runs          int
		queuedRuns    int
		tokens        int
		scheduleState string
	)
	if err := pool.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM deployment_definitions WHERE environment_id = $1),
		    (SELECT count(*) FROM schedules WHERE environment_id = $1),
		    (SELECT count(*) FROM workspaces WHERE environment_id = $1 AND deleted_at IS NULL),
		    (SELECT count(*) FROM sessions WHERE environment_id = $1),
		    (SELECT count(*) FROM runs WHERE environment_id = $1),
		    (SELECT count(*) FROM runs WHERE environment_id = $1 AND status = 'queued'),
		    (SELECT count(*) FROM tokens WHERE environment_id = $1),
		    (SELECT state FROM schedules WHERE id = $2)
	`, demoSeedEnvironmentID, demoSeedScheduleID).Scan(
		&definitions, &schedules, &workspaces, &sessions, &runs, &queuedRuns, &tokens, &scheduleState,
	); err != nil {
		t.Fatal(err)
	}
	if definitions != 3 || schedules != 1 || workspaces != 2 || sessions != 2 || runs != 4 || tokens != 2 {
		t.Fatalf(
			"definitions/schedules/workspaces/sessions/runs/tokens = %d/%d/%d/%d/%d/%d, want 3/1/2/2/4/2",
			definitions, schedules, workspaces, sessions, runs, tokens,
		)
	}
	if queuedRuns != 0 {
		t.Fatalf("queued demo runs = %d, want 0 so connected workers cannot pick up synthetic work", queuedRuns)
	}
	if scheduleState != "archived" {
		t.Fatalf("schedule state = %q, want archived", scheduleState)
	}

	var sessionRecords int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM session_records WHERE session_id = $1
	`, demoSeedSessionOpenID).Scan(&sessionRecords); err != nil {
		t.Fatal(err)
	}
	if sessionRecords != 3 {
		t.Fatalf("open session records = %d, want 3", sessionRecords)
	}
}

func TestDevSeedRestartPreservesEditsWithoutReseeding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := dbtest.Open(t).Pool
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping restart preservation test", serverVersion)
	}
	freshInit, err := migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
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
	if _, err := pool.Exec(ctx, `
		UPDATE tokens
		   SET state = 'completed',
		       result = '{"approved":true}'::jsonb,
		       completed_at = now()
		 WHERE id = $1
	`, demoSeedTokenPendingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schedules WHERE id = $1`, demoSeedScheduleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE environments SET current_deployment_id = NULL WHERE id = $1
	`, demoSeedEnvironmentID); err != nil {
		t.Fatal(err)
	}

	freshInit, err = migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if freshInit {
		t.Fatal("expected existing database on restart migrate")
	}

	var name, color, tokenState string
	var scheduleCount int
	var currentDeploymentID *string
	if err := pool.QueryRow(ctx, `
		SELECT name, color_hex FROM environments WHERE id = '00000000-0000-7000-8000-000000000401'
	`).Scan(&name, &color); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM tokens WHERE id = $1`, demoSeedTokenPendingID).Scan(&tokenState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schedules WHERE id = $1`, demoSeedScheduleID).Scan(&scheduleCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT current_deployment_id::text FROM environments WHERE id = $1
	`, demoSeedEnvironmentID).Scan(&currentDeploymentID); err != nil {
		t.Fatal(err)
	}
	if name != "Renamed Production" || color != "#22C55E" {
		t.Fatalf("environment after restart = %q/%q, want Renamed Production/#22C55E", name, color)
	}
	if tokenState != "completed" {
		t.Fatalf("token state after restart = %q, want completed", tokenState)
	}
	if scheduleCount != 0 {
		t.Fatalf("deleted schedule count after restart = %d, want 0", scheduleCount)
	}
	if currentDeploymentID != nil {
		t.Fatalf("current_deployment_id after restart = %v, want NULL", currentDeploymentID)
	}
}
