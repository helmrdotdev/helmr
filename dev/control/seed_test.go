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
		t.Skipf("Postgres %d does not provide uuidv7(); skipping dev seed integration test", serverVersion)
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

	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE org_id = '00000000-0000-0000-0000-000000000201'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 4 {
		t.Fatalf("seeded runs = %d, want 4", runs)
	}
	var placementLeaks int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = ANY($1::text[])
		   AND column_name IN ('worker_group_id', 'build_worker_group_id')
	`, []string{"deployments", "workspaces", "sessions", "runs", "session_runs"}).Scan(&placementLeaks); err != nil {
		t.Fatal(err)
	}
	if placementLeaks != 0 {
		t.Fatalf("logical placement columns = %d, want 0", placementLeaks)
	}
	var invalidArtifactMediaTypes int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM artifacts
		 WHERE id = ANY($1::uuid[])
		   AND media_type <> CASE kind
		       WHEN 'deployment_manifest' THEN 'application/vnd.helmr.deployment-manifest.v0+json'
		       WHEN 'task_bundle' THEN 'application/vnd.helmr.task-bundle.v0+proto'
		       WHEN 'sandbox_image' THEN 'application/vnd.helmr.sandbox-image.v0.oci-tar'
		       WHEN 'workspace_version' THEN 'application/vnd.helmr.workspace.v0.tar'
		       ELSE media_type
		   END
	`, []string{
		"00000000-0000-0000-0000-000000000502",
		"00000000-0000-0000-0000-000000000503",
		"00000000-0000-0000-0000-000000000504",
		"00000000-0000-0000-0000-000000000505",
	}).Scan(&invalidArtifactMediaTypes); err != nil {
		t.Fatal(err)
	}
	if invalidArtifactMediaTypes != 0 {
		t.Fatalf("seeded artifacts with invalid media types = %d, want 0", invalidArtifactMediaTypes)
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
