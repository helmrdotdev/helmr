package schema

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	assertWorkerSchema(t, dbctx, pool)
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

func assertWorkerSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var forbiddenRelations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class
		 WHERE relnamespace = 'public'::regnamespace
		   AND relname = ANY($1::text[])
	`, []string{"worker_commands", "run_checkpoint_restores", "worker_assignments", "runtime_routes"}).Scan(&forbiddenRelations); err != nil {
		t.Fatal(err)
	}
	if forbiddenRelations != 0 {
		t.Fatalf("forbidden worker relations = %d, want 0", forbiddenRelations)
	}
	var shapeColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='worker_instances' AND column_name = ANY($1::text[])`,
		[]string{"per_vm_cpu_millis", "per_vm_memory_bytes", "per_vm_workload_disk_bytes", "per_vm_scratch_bytes"}).Scan(&shapeColumns); err != nil {
		t.Fatal(err)
	}
	if shapeColumns != 4 {
		t.Fatalf("per-VM shape columns = %d, want 4", shapeColumns)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO regions (id, provider, provider_region, display_name) VALUES ('shape-region', 'test', 'shape-region', 'Shape Region');
		INSERT INTO worker_groups (id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints)
		VALUES ('shape-test', 'shape-region', 'shape-test', 'sha256:shape-test', ARRAY['sha256:shape-test']);
		INSERT INTO worker_instances (id, resource_id, worker_group_id, attestation_fingerprint, per_vm_cpu_millis, per_vm_memory_bytes, per_vm_workload_disk_bytes, per_vm_scratch_bytes)
		VALUES ('00000000-0000-0000-0000-000000000099', 'shape-test', 'shape-test', 'sha256:shape-test', 2000, 2147483648, 8589934592, 8589934592);
	`); err != nil {
		t.Fatal(err)
	}
	var exactFit, overShape bool
	if err := pool.QueryRow(ctx, `SELECT 4000::bigint <= per_vm_cpu_millis * 2, 4001::bigint <= per_vm_cpu_millis * 2 FROM worker_instances WHERE id='00000000-0000-0000-0000-000000000099'`).Scan(&exactFit, &overShape); err != nil {
		t.Fatal(err)
	}
	if !exactFit || overShape {
		t.Fatalf("per-executor exact/over shape fence = %t/%t", exactFit, overShape)
	}
	var beforeFull, replacementAllowed, replacementRefillsCap, withinVMSlots bool
	var selectedWorker string
	if err := pool.QueryRow(ctx, `
		WITH workers(id, max_runtime_starts, max_vm_slots) AS (VALUES ('worker-a',2,4),('worker-b',2,4)),
		runtimes(id, worker_id, observed_state) AS (VALUES ('r1','worker-a','ready'),('r2','worker-a','preparing'),('rb','worker-b','ready')),
		active_run_leases(runtime_id, state) AS (VALUES ('r1','running')),
		after_replacement AS (SELECT * FROM runtimes UNION ALL SELECT 'r3','worker-a','allocated')
		SELECT
			(SELECT count(*) FROM runtimes WHERE worker_id='worker-a') = 2,
			(SELECT count(*) FROM runtimes WHERE worker_id='worker-a' AND NOT EXISTS (SELECT 1 FROM active_run_leases WHERE runtime_id=runtimes.id AND state IN ('assigned','starting','running','checkpointing'))) < 2,
			(SELECT count(*) FROM after_replacement WHERE worker_id='worker-a' AND NOT EXISTS (SELECT 1 FROM active_run_leases WHERE runtime_id=after_replacement.id AND state IN ('assigned','starting','running','checkpointing'))) = 2,
			(SELECT count(*) FROM after_replacement WHERE worker_id='worker-a') <= 4,
			(SELECT id FROM workers WHERE id <> 'worker-a' AND (SELECT count(*) FROM runtimes WHERE worker_id=workers.id) < max_runtime_starts ORDER BY id LIMIT 1)
	`).Scan(&beforeFull, &replacementAllowed, &replacementRefillsCap, &withinVMSlots, &selectedWorker); err != nil {
		t.Fatal(err)
	}
	if !beforeFull || !replacementAllowed || !replacementRefillsCap || !withinVMSlots || selectedWorker != "worker-b" {
		t.Fatalf("prepared transition = before:%t replacement:%t refilled:%t slots:%t next:%q", beforeFull, replacementAllowed, replacementRefillsCap, withinVMSlots, selectedWorker)
	}
	assertPreparedSupplySerialization(t, ctx, pool)

	logicalTables := []string{"workspaces", "runs", "run_operations", "sessions", "session_runs", "session_continuation_requests", "streams", "stream_records", "run_waits", "run_checkpoints", "run_checkpoint_artifacts", "run_state_snapshots", "meter_events", "telemetry_outbox"}
	var placementLeaks int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = ANY($1::text[])
		   AND column_name = 'worker_group_id'
	`, logicalTables).Scan(&placementLeaks); err != nil {
		t.Fatal(err)
	}
	if placementLeaks != 0 {
		t.Fatalf("logical worker_group_id columns = %d, want 0", placementLeaks)
	}

	forbiddenColumns := map[string][]string{
		"runs":              {"dispatch_generation", "dispatch_attempt", "dispatch_message_id", "dispatch_lease_id", "workspace_mount_id", "worker_instance_id"},
		"runtime_instances": {"runtime_epoch", "state", "instance_token", "last_heartbeat_at", "owner_run_id", "owner_run_wait_id", "workspace_mount_id"},
		"worker_instances":  {"available_milli_cpu", "available_memory_mib", "heartbeat", "labels", "last_seen_at", "total_milli_cpu"},
	}
	for table, columns := range forbiddenColumns {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = $1
			   AND column_name = ANY($2::text[])
		`, table, columns).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("forbidden columns on %s = %d, want 0", table, count)
		}
	}

	requiredIndexes := []string{
		"deployment_build_leases_deployment_active_uidx",
		"run_leases_run_active_uidx",
		"run_leases_runtime_active_uidx",
		"runtime_instances_workspace_active_uidx",
		"runtime_instances_reservation_active_uidx",
		"network_slots_runtime_active_uidx",
		"workspace_mounts_workspace_active_uidx",
		"workspace_mounts_runtime_active_uidx",
		"workspace_leases_workspace_write_active_uidx",
		"run_waits_reserved_workspace_uidx",
	}
	var indexCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = ANY($1::text[])`, requiredIndexes).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != len(requiredIndexes) {
		t.Fatalf("required managed-worker indexes = %d, want %d", indexCount, len(requiredIndexes))
	}

	var enumLabels int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_enum JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
		 WHERE (pg_type.typname, pg_enum.enumlabel) IN (
		   ('worker_instance_state','registering'), ('worker_instance_state','lost'),
		   ('run_lease_state','checkpointing'), ('run_lease_state','expired'),
		   ('deployment_build_lease_state','succeeded'),
		   ('runtime_desired_state','closed'), ('runtime_observed_state','lost'),
		   ('worker_network_slot_state','quarantined'), ('run_wait_state','resuming')
		 )
	`).Scan(&enumLabels); err != nil {
		t.Fatal(err)
	}
	if enumLabels != 9 {
		t.Fatalf("managed-worker enum sentinel labels = %d, want 9", enumLabels)
	}
}

func assertPreparedSupplySerialization(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE prepared_supply_concurrency_test (id bigserial primary key, worker_group text not null, scope text not null, observed_state text not null, active_run_lease boolean not null);
		INSERT INTO prepared_supply_concurrency_test (worker_group,scope,observed_state,active_run_lease) VALUES
		('group-a','scope','running',true), ('group-a','scope','ready',false)`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, group := range []string{"group-a", "group-b"} {
		wg.Go(func() {
			<-start
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('scope',0))`); err == nil {
				_, err = tx.Exec(ctx, `INSERT INTO prepared_supply_concurrency_test (worker_group,scope,observed_state,active_run_lease)
					SELECT $1,'scope','allocated',false WHERE (SELECT count(*) FROM prepared_supply_concurrency_test WHERE scope='scope' AND observed_state IN ('allocated','preparing','ready') AND NOT active_run_lease) < 2`, group)
			}
			if err == nil {
				err = tx.Commit(ctx)
			}
			if err != nil {
				errs <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var prepared, occupied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE observed_state IN ('allocated','preparing','ready') AND NOT active_run_lease), count(*) FROM prepared_supply_concurrency_test`).Scan(&prepared, &occupied); err != nil {
		t.Fatal(err)
	}
	if prepared != 2 || occupied != 3 {
		t.Fatalf("serialized prepared supply = prepared:%d occupied:%d, want 2/3", prepared, occupied)
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
		       'workspace_process_stream_chunks'
		   )
		   AND column_name = 'expires_at'
		   AND is_nullable = 'NO'
	`).Scan(&boundedHotTables); err != nil {
		t.Fatal(err)
	}
	if boundedHotTables != 1 {
		t.Fatalf("bounded telemetry hot payload tables = %d, want 1", boundedHotTables)
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
			   AND table_name = 'workspace_process_stream_chunks'
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
			 WHERE pg_type.typname = 'workspace_process_stream'
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
				'workspace_process_stream_chunks_no_overlap'
			 )
		   AND contype = 'x'
	`).Scan(&constraintCount); err != nil {
		t.Fatal(err)
	}
	if constraintCount != 1 {
		t.Fatalf("workspace stream overlap exclusion constraints = %d, want 1", constraintCount)
	}
	var hasActiveResourceIndex bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_indexes
			 WHERE schemaname = 'public'
			   AND tablename = 'workspace_process_operations'
			   AND indexname = 'workspace_process_operations_active_process_idx'
			   AND indexdef ILIKE '%WHERE%state%queued%'
			   AND indexdef ILIKE '%process_id%'
		)
	`).Scan(&hasActiveResourceIndex); err != nil {
		t.Fatal(err)
	}
	if !hasActiveResourceIndex {
		t.Fatal("workspace process operations must prevent duplicate active process dispatch")
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
