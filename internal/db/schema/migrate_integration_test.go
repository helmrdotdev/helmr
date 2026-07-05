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
		testUpWithPostgres(t, ctx, dsn, false)
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
	testUpWithPostgres(t, ctx, dsn, true)
}

func testUpWithPostgres(t *testing.T, ctx context.Context, dsn string, verifyDown bool) {
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
	assertTelemetrySchema(t, dbctx, pool)
	if !verifyDown {
		return
	}
	if err := Down(dbctx, dsn); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if err := pool.QueryRow(dbctx, `SELECT to_regclass('public.runs') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("runs table still exists after down migration")
	}
}

func assertTelemetrySchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var legacyPayloadRelations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_class
		  JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
		 WHERE pg_namespace.nspname = 'public'
		   AND pg_class.relname IN ('events', 'run_log_chunks')
		   AND pg_class.relkind IN ('r', 'p', 'v', 'm')
	`).Scan(&legacyPayloadRelations); err != nil {
		t.Fatal(err)
	}
	if legacyPayloadRelations != 0 {
		t.Fatalf("legacy telemetry payload relations = %d, want 0", legacyPayloadRelations)
	}
	var boundedHotTables int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name IN (
		       'event_hot_payloads',
		       'run_log_hot_chunks',
		       'workspace_exec_stream_chunks',
		       'workspace_pty_stream_chunks'
		   )
		   AND column_name = 'expires_at'
		   AND is_nullable = 'NO'
	`).Scan(&boundedHotTables); err != nil {
		t.Fatal(err)
	}
	if boundedHotTables != 4 {
		t.Fatalf("bounded telemetry hot payload tables = %d, want 4", boundedHotTables)
	}
	var oldUsageEnums int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_type
		 WHERE typname LIKE 'run\_usage\_event\_%' ESCAPE '\'
	`).Scan(&oldUsageEnums); err != nil {
		t.Fatal(err)
	}
	if oldUsageEnums != 0 {
		t.Fatalf("legacy usage enum types = %d, want 0", oldUsageEnums)
	}
	var workerGroupPropagationFunctions int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_proc
		 WHERE proname LIKE 'set\_%\_worker\_group\_id' ESCAPE '\'
	`).Scan(&workerGroupPropagationFunctions); err != nil {
		t.Fatal(err)
	}
	if workerGroupPropagationFunctions != 0 {
		t.Fatalf("worker group propagation functions = %d, want 0", workerGroupPropagationFunctions)
	}
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
			   AND tablename = 'workspace_operations'
			   AND indexname = 'workspace_operations_active_resource_idx'
			   AND indexdef ILIKE '%WHERE%state%queued%'
			   AND indexdef ILIKE '%resource_id IS NOT NULL%'
		)
	`).Scan(&hasActiveResourceIndex); err != nil {
		t.Fatal(err)
	}
	if !hasActiveResourceIndex {
		t.Fatal("workspace operations must prevent duplicate active resource dispatch")
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
