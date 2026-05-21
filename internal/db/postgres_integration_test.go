package db_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newPostgresTestDB(t *testing.T, ctx context.Context) (*db.Queries, *pgxpool.Pool) {
	t.Helper()
	if dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL")); dsn != "" {
		return newExternalPostgresTestDB(t, ctx, dsn, "schema/migrations/*.up.sql")
	}
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping Postgres integration test", name)
		}
	}
	tmp, err := os.MkdirTemp("", "helmr-pg-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tmp)
	})
	dataDir := filepath.Join(tmp, "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freePort(t)
	logPath := filepath.Join(tmp, "postgres.log")
	start := exec.Command("pg_ctl", "-D", dataDir, "-l", logPath, "-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})

	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	dbctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(dbctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	applyPostgresTestMigrations(t, dbctx, pool, "schema/migrations/*.up.sql")
	pool.Close()
	registeredPool, err := pgxpool.New(dbctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registeredPool.Close)
	return db.New(registeredPool), registeredPool
}

func seedPostgresTestOrganization(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO organizations (id, name, slug)
VALUES ($1, 'Test Organization', 'test-organization')
ON CONFLICT (id) DO NOTHING
`, orgID); err != nil {
		t.Fatal(err)
	}
}

func seedPostgresTestDefaultScope(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, orgID pgtype.UUID) db.GetDefaultProjectEnvironmentRow {
	t.Helper()
	seedPostgresTestOrganization(t, ctx, pool, orgID)
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err == nil {
		seedPostgresTestDefaultWorkerPool(t, ctx, queries, orgID)
		return scope
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		Slug:          "main",
		Name:          "Main",
		EnvironmentID: ids.ToPG(ids.New()),
	}); err != nil {
		t.Fatal(err)
	}
	scope, err = queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresTestDefaultWorkerPool(t, ctx, queries, orgID)
	return scope
}

func seedPostgresTestDefaultWorkerPool(t *testing.T, ctx context.Context, queries *db.Queries, orgID pgtype.UUID) db.WorkerPool {
	t.Helper()
	pools, err := queries.ListWorkerPools(ctx, db.ListWorkerPoolsParams{
		OrgID:    orgID,
		RowLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, workerPool := range pools {
		if workerPool.Slug == "default" {
			return workerPool
		}
	}
	workerPool, err := queries.CreateWorkerPool(ctx, db.CreateWorkerPoolParams{
		ID:               ids.ToPG(ids.New()),
		OrgID:            orgID,
		Slug:             "default",
		Name:             "Default",
		ProvisioningMode: db.WorkerPoolProvisioningModeCustomerManaged,
		QueueName:        "default",
		Region:           "",
		Capabilities:     []byte(`{}`),
		Metadata:         []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return workerPool
}

func seedPostgresTestWorkerRegistrationToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, orgID pgtype.UUID, tokenHash []byte) {
	t.Helper()
	seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	pools, err := queries.ListWorkerPools(ctx, db.ListWorkerPoolsParams{
		OrgID:    orgID,
		RowLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) == 0 {
		t.Fatal("default scope has no worker pool")
	}
	if _, err := queries.UpsertWorkerRegistrationToken(ctx, db.UpsertWorkerRegistrationTokenParams{
		ID:           ids.ToPG(ids.New()),
		OrgID:        orgID,
		WorkerPoolID: pools[0].ID,
		TokenHash:    tokenHash,
	}); err != nil {
		t.Fatal(err)
	}
}

func newExternalPostgresTestDB(t *testing.T, ctx context.Context, dsn string, migrationsGlob string) (*db.Queries, *pgxpool.Pool) {
	t.Helper()
	adminCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(adminCtx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "helmr_test_" + strings.ReplaceAll(ids.New().String(), "-", "")
	dbIdentifier := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(adminCtx, "CREATE DATABASE "+dbIdentifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+dbIdentifier)
		admin.Close()
	})

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = dbName
	dbctx, dbcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dbcancel()
	pool, err := pgxpool.NewWithConfig(dbctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	applyPostgresTestMigrations(t, dbctx, pool, migrationsGlob)
	pool.Close()
	registeredPool, err := pgxpool.NewWithConfig(dbctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registeredPool.Close)
	return db.New(registeredPool), registeredPool
}

func applyPostgresTestMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, migrationsGlob string) {
	t.Helper()
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping Postgres integration test", serverVersion)
	}
	migrations, err := filepath.Glob(migrationsGlob)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func pgTime(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func pgText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: true}
}
