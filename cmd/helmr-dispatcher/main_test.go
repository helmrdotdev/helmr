package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFleetConfigurationDoesNotLoadAWSWhenDisabled(t *testing.T) {
	original := loadAWSConfig
	t.Cleanup(func() { loadAWSConfig = original })
	called := false
	loadAWSConfig = func(context.Context, ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		called = true
		return aws.Config{}, nil
	}
	controllers, pools, err := configureFleetControllers(context.Background(), config.Dispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if called || len(controllers) != 0 || len(pools) != 0 {
		t.Fatalf("called=%t controllers=%d pools=%d", called, len(controllers), len(pools))
	}
}

func TestExplicitManagedFleetFailsStartupWhenAWSConfigCannotLoad(t *testing.T) {
	original := loadAWSConfig
	t.Cleanup(func() { loadAWSConfig = original })
	loadAWSConfig = func(context.Context, ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		return aws.Config{}, errors.New("no credentials")
	}
	_, _, err := configureFleetControllers(context.Background(), config.Dispatcher{
		WorkerFleets: []config.WorkerFleet{{GroupID: "run", Role: "run", ASGName: "run-asg"}},
	})
	if err == nil || !strings.Contains(err.Error(), "load AWS config") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunStartsAndStopsWithConfiguredDependencies(t *testing.T) {
	ctx := context.Background()
	databaseURL := newSmokeDatabase(t, ctx)
	redisServer := miniredis.RunT(t)

	t.Setenv("HELMR_DATABASE_URL", databaseURL)
	t.Setenv("HELMR_REDIS_URL", "redis://"+redisServer.Addr()+"/0")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:1")
	t.Setenv("HELMR_SCHEDULE_POLL_INTERVAL", "50ms")
	t.Setenv("HELMR_SCHEDULE_CLAIM_LEASE", "100ms")
	t.Setenv("HELMR_WORKSPACE_FENCING_KEY_FINGERPRINT", "sha256:29f47c71b2eb74ea02b312a6c045e1497cd81313f1bdc037a5529139ea0a0a26")
	t.Setenv("HELMR_WORKSPACE_FENCING_KEYS", `{"sha256:29f47c71b2eb74ea02b312a6c045e1497cd81313f1bdc037a5529139ea0a0a26":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="}`)

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
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping dispatcher smoke test", serverVersion)
	}
	if err := schema.Up(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	return databaseURL
}
