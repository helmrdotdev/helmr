package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunStartsAndStopsWithConfiguredDependencies(t *testing.T) {
	ctx := context.Background()
	databaseURL := newSmokeDatabase(t, ctx)
	t.Setenv("DATABASE_URL", databaseURL)
	ready := make(chan struct{}, 1)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		select {
		case ready <- struct{}{}:
		default:
		}
	}))
	defer cp.Close()
	t.Setenv("CONTROL_PLANE_URL", cp.URL)
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:1")
	t.Setenv("COMPUTER_FENCING_KEY", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=")
	t.Setenv("ENCRYPTION_KEY", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=")

	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- runDispatcher(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("dispatcher run returned before cancel: %v", err)
		}
		t.Fatal("dispatcher run returned before cancel")
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatcher did not check Control Plane readiness")
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
	// Administrative connection for database DDL.
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "helmr_smoke_" + strings.ReplaceAll(uuid.NewV7().String(), "-", "")
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
	// Probe connection for the server version check.
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
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping dispatcher smoke test", serverVersion)
	}
	if err := schema.Up(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	return databaseURL
}
