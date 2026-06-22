package schema

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUpWithPostgres(t *testing.T) {
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL")); dsn != "" {
		testUpWithExternalPostgres(t, ctx, dsn)
		return
	}
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping Postgres migration test", name)
		}
	}
	tmp, err := os.MkdirTemp("", "helmr-schema-pg-*")
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
	port := freePostgresPort(t)
	logPath := filepath.Join(tmp, "postgres.log")
	start := exec.Command("pg_ctl", "-D", dataDir, "-l", logPath, "-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})
	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	testUpWithExternalPostgres(t, ctx, dsn)
}

func testUpWithExternalPostgres(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	dbctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(dbctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var serverVersion int
	if err := pool.QueryRow(dbctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping migration test", serverVersion)
	}
	if err := Up(dbctx, dsn); err != nil {
		t.Fatal(err)
	}
	if err := Up(dbctx, dsn); err != nil {
		t.Fatalf("second migration should be a no-op: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(dbctx, `SELECT to_regclass('public.runs') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("runs table was not created")
	}
	assertWorkspaceStreamSchema(t, dbctx, pool)
}

func assertWorkspaceStreamSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var hasSequence bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM information_schema.columns
			 WHERE table_schema = 'public'
			   AND table_name IN ('workspace_exec_stream_chunks', 'workspace_pty_stream_chunks')
			   AND column_name = 'sequence'
		)
	`).Scan(&hasSequence); err != nil {
		t.Fatal(err)
	}
	if hasSequence {
		t.Fatal("workspace stream chunks must use offset cursors, not sequence columns")
	}
	var hasResize bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_enum
			  JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
			 WHERE pg_type.typname = 'workspace_pty_stream'
			   AND pg_enum.enumlabel = 'resize'
		)
	`).Scan(&hasResize); err != nil {
		t.Fatal(err)
	}
	if hasResize {
		t.Fatal("PTY resize must be a control verb, not a byte stream")
	}
	var constraintCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_constraint
		 WHERE conname IN (
		 	'workspace_exec_stream_chunks_no_overlap',
		 	'workspace_pty_stream_chunks_no_overlap'
		 )
		   AND contype = 'x'
	`).Scan(&constraintCount); err != nil {
		t.Fatal(err)
	}
	if constraintCount != 2 {
		t.Fatalf("workspace stream overlap exclusion constraints = %d, want 2", constraintCount)
	}
	var hasActiveResourceIndex bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_indexes
			 WHERE schemaname = 'public'
			   AND tablename = 'workspace_materialization_operations'
			   AND indexname = 'workspace_materialization_operations_active_resource_idx'
			   AND indexdef ILIKE '%WHERE%state%queued%'
			   AND indexdef ILIKE '%resource_id IS NOT NULL%'
		)
	`).Scan(&hasActiveResourceIndex); err != nil {
		t.Fatal(err)
	}
	if !hasActiveResourceIndex {
		t.Fatal("workspace materialization operations must prevent duplicate active resource dispatch")
	}
}

func freePostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
