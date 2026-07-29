package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDevSeedWithFreshPostgres(t *testing.T) {
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping dev seed integration test", name)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	if output, err := exec.CommandContext(ctx, "initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freeDevPostgresPort(t)
	logPath := filepath.Join(tmp, "postgres.log")
	start := exec.CommandContext(ctx, "pg_ctl", "-D", dataDir, "-l", logPath, "-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})

	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
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
	if err := region.Ensure(ctx, db.New(pool), region.BootstrapConfig{
		RegionID:          "dev-local",
		DefaultRegionID:   "dev-local",
		Provider:          "local",
		ProviderRegion:    "local",
		RegionDisplayName: "Local",
	}); err != nil {
		t.Fatalf("bootstrap local region: %v", err)
	}
	cfg := devConfig{defaultRegionID: "dev-local"}
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

func freeDevPostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
