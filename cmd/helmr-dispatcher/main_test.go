package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunStartsAndStopsWithConfiguredDependencies(t *testing.T) {
	ctx := context.Background()
	databaseURL := newSmokeDatabase(t, ctx)
	redisServer := miniredis.RunT(t)

	t.Setenv("HELMR_DATABASE_URL", databaseURL)
	t.Setenv("HELMR_REDIS_URL", "redis://"+redisServer.Addr()+"/0")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:1")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_PUBLIC_URL", "http://127.0.0.1:8080")
	t.Setenv("HELMR_EMAIL_PROVIDER", "none")
	t.Setenv("HELMR_SCHEDULE_REPAIR_EVERY", "50ms")
	t.Setenv("HELMR_SCHEDULE_REPAIR_LOOKAHEAD", "100ms")
	t.Setenv("HELMR_SCHEDULE_LEASE", "100ms")

	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- run(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("dispatcher run returned before cancel: %v", err)
		}
		t.Fatal("dispatcher run returned before cancel")
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("dispatcher run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dispatcher run did not stop")
	}
}

func newSmokeDatabase(t *testing.T, ctx context.Context) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("HELMR_TEST_DATABASE_URL is required for whole-binary smoke tests")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "helmr_smoke_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
	dbIdentifier := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbIdentifier); err != nil {
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
	databaseURL := config.ConnString()
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(checkCtx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var serverVersion int
	if err := pool.QueryRow(checkCtx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping dispatcher smoke test", serverVersion)
	}
	if err := schema.Up(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	return databaseURL
}
