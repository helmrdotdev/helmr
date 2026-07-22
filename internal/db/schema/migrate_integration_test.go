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

	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
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
	assertDeploymentBuildCapacitySchema(t, dbctx, pool)
	assertDeploymentDefinitionAuthority(t, dbctx, pool)
	assertWorkspaceVersionAuthority(t, dbctx, pool)
	assertIdempotencyClaimCollectionIndexes(t, dbctx, pool)
	assertExecutionAttachmentConstraints(t, dbctx, pool)
	assertRunWaitHandoffAuthority(t, dbctx, pool)
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
	if err := Up(dbctx, dsn); err != nil {
		t.Fatalf("migration after down failed: %v", err)
	}
	if err := pool.QueryRow(dbctx, `SELECT to_regclass('public.runs') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("runs table was not recreated after down migration")
	}
	assertDeploymentDefinitionAuthority(t, dbctx, pool)
	assertWorkspaceVersionAuthority(t, dbctx, pool)
	assertIdempotencyClaimCollectionIndexes(t, dbctx, pool)
	assertExecutionAttachmentConstraints(t, dbctx, pool)
	assertRunWaitHandoffAuthority(t, dbctx, pool)
}

func assertExecutionAttachmentConstraints(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	constraints := map[string]string{
		"run_leases_network_slot_id_fkey":                     "FOREIGN KEY (network_slot_id) REFERENCES worker_network_slots(id)",
		"workspace_leases_owner_run_lease_fk":                 "FOREIGN KEY (workspace_id, runtime_instance_id, owner_run_lease_id) REFERENCES run_leases(workspace_id, runtime_instance_id, id)",
		"runtime_instances_restore_checkpoint_workspace_fkey": "FOREIGN KEY (restore_checkpoint_id, workspace_id) REFERENCES run_checkpoints(id, workspace_id)",
		"runtime_instances_restore_checkpoint_execution_fkey": "FOREIGN KEY (restore_checkpoint_id, reserved_run_id, reserved_attempt_number, workspace_id) REFERENCES run_checkpoints(id, run_id, attempt_number, workspace_id)",
	}
	for name, want := range constraints {
		var got string
		if err := pool.QueryRow(ctx, `
SELECT pg_get_constraintdef(oid)
  FROM pg_constraint
 WHERE conname = $1
`, name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(got, want) {
			t.Fatalf("%s = %q, want to contain %q", name, got, want)
		}
	}
	var processFence bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
     FROM pg_constraint
     WHERE conrelid = 'runtime_instances'::regclass
       AND contype = 'c'
       AND pg_get_constraintdef(oid) LIKE '%restore_checkpoint_id IS NULL%'
       AND pg_get_constraintdef(oid) LIKE '%reserved_process_id IS NULL%'
)
`).Scan(&processFence); err != nil {
		t.Fatal(err)
	}
	if !processFence {
		t.Fatal("runtime restore provenance does not fence direct Process reservation")
	}
}

func assertRunWaitHandoffAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	indexes := map[string]bool{
		"run_waits_handoff_runtime_active_idx": false,
		"run_waits_handoff_mount_active_idx":   false,
		"run_waits_handoff_child_active_uidx":  true,
	}
	for name, unique := range indexes {
		var definition string
		if err := pool.QueryRow(ctx, `
SELECT indexdef
  FROM pg_indexes
 WHERE schemaname = 'public'
   AND indexname = $1
`, name).Scan(&definition); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got := strings.Contains(definition, "CREATE UNIQUE INDEX"); got != unique {
			t.Fatalf("%s unique = %t, want %t", name, got, unique)
		}
	}

	if _, err := pool.Exec(ctx, `
DROP TABLE IF EXISTS pg_temp.run_wait_handoff_shapes;
CREATE TEMP TABLE run_wait_handoff_shapes
    (LIKE run_waits INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS);

INSERT INTO run_wait_handoff_shapes (
    environment_id, run_id, workspace_id, kind, suspension_state,
    expected_run_state_version, attempt_number, current_run_lease_id,
    resume_attach_id, child_parent_owned, child_target_declared_id,
    child_claim_id, child_request
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003',
    'child', 'checkpointing', 1, 1,
    '00000000-0000-0000-0000-000000000004',
    '00000000-0000-0000-0000-000000000005',
    true, 'child', '00000000-0000-0000-0000-000000000006', '{}'::jsonb
);

INSERT INTO run_wait_handoff_shapes (
    environment_id, run_id, workspace_id, kind, suspension_state,
    expected_run_state_version, attempt_number, prior_run_lease_id,
    suspend_checkpoint_id, resume_attach_id, child_run_id, child_parent_owned,
    child_target_declared_id, child_claim_id, child_request,
    base_workspace_version_id, base_workspace_content_digest,
    handoff_runtime_instance_id, handoff_workspace_mount_id,
    handoff_mount_generation, ownership_generation,
    parent_writer_generation
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003',
    'child', 'parked', 1, 1,
    '00000000-0000-0000-0000-000000000004',
    '00000000-0000-0000-0000-00000000000e',
    '00000000-0000-0000-0000-000000000005',
    '00000000-0000-0000-0000-000000000007',
    true, 'child', '00000000-0000-0000-0000-000000000006', '{}'::jsonb,
    '00000000-0000-0000-0000-000000000008', 'sha256:base',
    '00000000-0000-0000-0000-000000000009',
    '00000000-0000-0000-0000-00000000000a',
    1, 1, 1
);

INSERT INTO run_wait_handoff_shapes (
    environment_id, run_id, workspace_id, kind, condition_state,
    condition_terminal_at, suspension_state, expected_run_state_version,
    attempt_number, prior_run_lease_id, suspend_checkpoint_id,
    resume_attach_id, child_run_id,
    child_parent_owned, child_target_declared_id, child_claim_id, child_request,
    base_workspace_version_id, base_workspace_content_digest,
    child_result_version_id, resume_workspace_version_id,
    handoff_runtime_instance_id, handoff_workspace_mount_id,
    handoff_mount_generation, ownership_generation, parent_writer_generation,
    child_writer_generation, handoff_resume_checkpoint_id
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003',
    'child', 'completed', now(), 'resume_pending', 1, 1,
    '00000000-0000-0000-0000-000000000004',
    '00000000-0000-0000-0000-00000000000e',
    '00000000-0000-0000-0000-000000000005',
    '00000000-0000-0000-0000-000000000007',
    true, 'child', '00000000-0000-0000-0000-000000000006', '{}'::jsonb,
    '00000000-0000-0000-0000-000000000008', 'sha256:base',
    '00000000-0000-0000-0000-00000000000b',
    '00000000-0000-0000-0000-00000000000b',
    '00000000-0000-0000-0000-000000000009',
    '00000000-0000-0000-0000-00000000000a',
    1, 1, 1, 2,
    '00000000-0000-0000-0000-00000000000c'
);

INSERT INTO run_wait_handoff_shapes (
    environment_id, run_id, workspace_id, kind, condition_state,
    condition_terminal_at, condition_reason_code, suspension_state,
    expected_run_state_version, attempt_number, prior_run_lease_id,
    suspend_checkpoint_id, resume_attach_id, child_run_id, child_parent_owned,
    child_target_declared_id, child_claim_id, child_request,
    base_workspace_version_id, base_workspace_content_digest,
    resume_workspace_version_id, handoff_runtime_instance_id,
    handoff_workspace_mount_id, handoff_mount_generation,
    ownership_generation, parent_writer_generation
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003',
    'child', 'failed', now(), 'child_failed', 'resume_pending', 1, 1,
    '00000000-0000-0000-0000-000000000004',
    '00000000-0000-0000-0000-00000000000e',
    '00000000-0000-0000-0000-000000000005',
    '00000000-0000-0000-0000-000000000007',
    true, 'child', '00000000-0000-0000-0000-000000000006', '{}'::jsonb,
    '00000000-0000-0000-0000-000000000008', 'sha256:base',
    '00000000-0000-0000-0000-000000000008',
    '00000000-0000-0000-0000-000000000009',
    '00000000-0000-0000-0000-00000000000a',
    1, 1, 1
);

INSERT INTO run_wait_handoff_shapes (
    environment_id, run_id, workspace_id, kind, condition_state,
    condition_terminal_at, suspension_state, expected_run_state_version,
    attempt_number, current_run_lease_id, prior_run_lease_id,
    suspend_checkpoint_id, resume_attach_id, child_run_id, child_parent_owned,
    child_target_declared_id, child_claim_id,
    child_request, base_workspace_version_id, base_workspace_content_digest,
    child_result_version_id, resume_workspace_version_id,
    handoff_runtime_instance_id, handoff_workspace_mount_id,
    handoff_mount_generation, ownership_generation, parent_writer_generation,
    child_writer_generation, resume_writer_generation,
    handoff_resume_checkpoint_id
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003',
    'child', 'completed', now(), 'resuming', 1, 1,
    '00000000-0000-0000-0000-00000000000d',
    '00000000-0000-0000-0000-000000000004',
    '00000000-0000-0000-0000-00000000000e',
    '00000000-0000-0000-0000-000000000005',
    '00000000-0000-0000-0000-000000000007',
    true, 'child', '00000000-0000-0000-0000-000000000006', '{}'::jsonb,
    '00000000-0000-0000-0000-000000000008', 'sha256:base',
    '00000000-0000-0000-0000-00000000000b',
    '00000000-0000-0000-0000-00000000000b',
    '00000000-0000-0000-0000-000000000009',
    '00000000-0000-0000-0000-00000000000a',
    1, 1, 1, 2, 3,
    '00000000-0000-0000-0000-00000000000c'
);
`); err != nil {
		t.Fatalf("insert valid Run Wait handoff stages: %v", err)
	}
	tag, err := pool.Exec(ctx, `
UPDATE run_wait_handoff_shapes
   SET child_writer_generation = 2
 WHERE condition_state = 'pending'
   AND child_run_id IS NOT NULL
`)
	if err != nil {
		t.Fatalf("advance Run Wait to child-granted stage: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("child-granted Run Wait rows = %d, want 1", tag.RowsAffected())
	}

	if _, err := pool.Exec(ctx, `
UPDATE run_wait_handoff_shapes
   SET resume_workspace_version_id = NULL
 WHERE condition_state = 'completed'
   AND suspension_state = 'resume_pending'
`); err == nil {
		t.Fatal("successful handoff without a resume Workspace version was accepted")
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_wait_handoff_shapes
   SET resume_writer_generation = 4
 WHERE condition_state = 'completed'
   AND suspension_state = 'resume_pending'
`); err == nil {
		t.Fatal("parent writer generation before regrant was accepted")
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO run_wait_handoff_shapes (
    environment_id, run_id, workspace_id, kind, suspension_state,
    expected_run_state_version, attempt_number, prior_run_lease_id,
    suspend_checkpoint_id, resume_attach_id, child_run_id, child_parent_owned,
    child_target_declared_id, child_claim_id, child_request,
    base_workspace_version_id, base_workspace_content_digest,
    handoff_runtime_instance_id, handoff_workspace_mount_id,
    handoff_mount_generation, ownership_generation,
    parent_writer_generation, resume_writer_generation
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003',
    'child', 'parked', 1, 1,
    '00000000-0000-0000-0000-000000000004',
    '00000000-0000-0000-0000-00000000000e',
    '00000000-0000-0000-0000-000000000005',
    '00000000-0000-0000-0000-000000000007',
    true, 'child', '00000000-0000-0000-0000-000000000006', '{}'::jsonb,
    '00000000-0000-0000-0000-000000000008', 'sha256:base',
    '00000000-0000-0000-0000-000000000009',
    '00000000-0000-0000-0000-00000000000a',
    1, 1, 1, 2
);
`); err == nil {
		t.Fatal("partial same-Workspace parent regrant shape was accepted")
	}
	if _, err := pool.Exec(ctx, `DROP TABLE run_wait_handoff_shapes`); err != nil {
		t.Fatalf("drop Run Wait handoff shape table: %v", err)
	}
}

func assertIdempotencyClaimCollectionIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	required := []string{
		"idempotency_claims_live_expiry_idx",
		"idempotency_claims_retired_idx",
		"runs_claim_idx",
		"actor_records_claim_idx",
		"run_stream_records_claim_lookup_idx",
		"run_waits_child_claim_idx",
	}
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_indexes
		 WHERE schemaname = 'public'
		   AND indexname = ANY($1::text[])
	`, required).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(required) {
		t.Fatalf("idempotency claim collection indexes = %d, want %d", count, len(required))
	}
}

func assertWorkspaceVersionAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var states []string
	if err := pool.QueryRow(ctx, `
		SELECT array_agg(enumlabel ORDER BY enumsortorder)
		  FROM pg_enum
		  JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
		 WHERE pg_type.typname = 'workspace_version_state'
	`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(states, ","), "private,committed,discarded"; got != want {
		t.Fatalf("workspace version states = %q, want %q", got, want)
	}
	var authorityColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'workspace_versions'
		   AND column_name = ANY($1::text[])
	`, []string{
		"parent_version_id",
		"artifact_id",
		"artifact_kind",
		"content_digest",
		"entry_count",
		"source_workspace_lease_id",
		"ownership_generation",
		"writer_generation",
		"published_at",
		"discarded_at",
	}).Scan(&authorityColumns); err != nil {
		t.Fatal(err)
	}
	if authorityColumns != 10 {
		t.Fatalf("workspace version authority columns = %d, want 10", authorityColumns)
	}
	var legacyColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'workspace_versions'
		   AND column_name = ANY($1::text[])
	`, []string{
		"source_workspace_mount_id",
		"source_write_lease_id",
		"produced_by_run_id",
		"artifact_encoding",
		"artifact_entry_count",
		"message",
		"error",
		"promoted_at",
	}).Scan(&legacyColumns); err != nil {
		t.Fatal(err)
	}
	if legacyColumns != 0 {
		t.Fatalf("legacy workspace version columns = %d, want 0", legacyColumns)
	}
	var emptyTreeCheck bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_constraint
			 WHERE conrelid = 'workspace_versions'::regclass
			   AND contype = 'c'
			   AND pg_get_constraintdef(oid) LIKE '%' || $1 || '%'
		)
	`, workspace.CanonicalEmptyTreeDigest).Scan(&emptyTreeCheck); err != nil {
		t.Fatal(err)
	}
	if !emptyTreeCheck {
		t.Fatal("workspace generation zero does not pin the canonical empty tree digest")
	}
	var oneRoot bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_indexes
			 WHERE schemaname = 'public'
			   AND tablename = 'workspace_versions'
			   AND indexdef LIKE 'CREATE UNIQUE INDEX%'
			   AND indexdef LIKE '%(workspace_id)%'
			   AND indexdef LIKE '%WHERE (parent_version_id IS NULL)%'
		)
	`).Scan(&oneRoot); err != nil {
		t.Fatal(err)
	}
	if !oneRoot {
		t.Fatal("workspace versions do not enforce one generation-zero root")
	}
	var fencedSource bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_constraint
			 WHERE conrelid = 'workspace_versions'::regclass
			   AND contype = 'f'
			   AND pg_get_constraintdef(oid) LIKE '%source_workspace_lease_id, ownership_generation, writer_generation%'
		)
	`).Scan(&fencedSource); err != nil {
		t.Fatal(err)
	}
	if !fencedSource {
		t.Fatal("workspace versions do not bind their source lease and writer fence")
	}
	var artifactKindBinding bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_constraint
			 WHERE conrelid = 'workspace_versions'::regclass
			   AND contype = 'f'
			   AND pg_get_constraintdef(oid) LIKE '%artifact_id, artifact_kind%'
		)
	`).Scan(&artifactKindBinding); err != nil {
		t.Fatal(err)
	}
	if !artifactKindBinding {
		t.Fatal("workspace versions do not bind their artifact kind")
	}
	var mountProjectionColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'workspace_mounts'
		   AND column_name = ANY($1::text[])
	`, []string{
		"image_artifact_id",
		"rootfs_digest",
		"workspace_artifact_id",
		"workspace_artifact_digest",
		"workspace_mount_path",
		"runtime_abi",
		"guestd_abi",
		"adapter_abi",
	}).Scan(&mountProjectionColumns); err != nil {
		t.Fatal(err)
	}
	if mountProjectionColumns != 0 {
		t.Fatalf("workspace mount copied projections = %d, want 0", mountProjectionColumns)
	}
	var runtimeProjectionColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'runtime_instances'
		   AND column_name = ANY($1::text[])
	`, []string{
		"rootfs_digest",
		"runtime_abi",
		"guestd_abi",
		"adapter_abi",
	}).Scan(&runtimeProjectionColumns); err != nil {
		t.Fatal(err)
	}
	if runtimeProjectionColumns != 0 {
		t.Fatalf("runtime instance copied profile fields = %d, want 0", runtimeProjectionColumns)
	}
}

func assertDeploymentBuildCapacitySchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var cpuDefault, memoryDefault, workloadDefault, scratchDefault, executorsDefault string
	if err := pool.QueryRow(ctx, `
		SELECT
		    max(column_default) FILTER (WHERE column_name = 'build_requested_cpu_millis'),
		    max(column_default) FILTER (WHERE column_name = 'build_requested_memory_bytes'),
		    max(column_default) FILTER (WHERE column_name = 'build_requested_workload_disk_bytes'),
		    max(column_default) FILTER (WHERE column_name = 'build_requested_scratch_bytes'),
		    max(column_default) FILTER (WHERE column_name = 'build_requested_executors')
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'deployments'
	`).Scan(&cpuDefault, &memoryDefault, &workloadDefault, &scratchDefault, &executorsDefault); err != nil {
		t.Fatal(err)
	}
	if cpuDefault != "3000" || memoryDefault != "'4294967296'::bigint" || workloadDefault != "0" ||
		scratchDefault != "'34359738368'::bigint" || executorsDefault != "1" {
		t.Fatalf(
			"build defaults = cpu:%s memory:%s workload:%s scratch:%s executors:%s",
			cpuDefault,
			memoryDefault,
			workloadDefault,
			scratchDefault,
			executorsDefault,
		)
	}
	var cacheColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name IN ('deployments', 'deployment_build_leases')
		   AND column_name = ANY($1::text[])
	`, []string{
		"build_requested_build_cache_bytes",
		"build_requested_artifact_cache_bytes",
		"requested_build_cache_bytes",
		"requested_artifact_cache_bytes",
	}).Scan(&cacheColumns); err != nil {
		t.Fatal(err)
	}
	if cacheColumns != 0 {
		t.Fatalf("per-delivery cache columns = %d, want 0", cacheColumns)
	}
	var executorConstraints int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_constraint
		 WHERE conrelid IN ('deployments'::regclass, 'deployment_build_leases'::regclass)
		   AND contype = 'c'
		   AND pg_get_constraintdef(oid) ~ '(build_requested_executors|requested_build_executors) = 1'
	`).Scan(&executorConstraints); err != nil {
		t.Fatal(err)
	}
	if executorConstraints != 2 {
		t.Fatalf("exact-one build executor constraints = %d, want 2", executorConstraints)
	}
}

func assertDeploymentDefinitionAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var definitionTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.deployment_definitions') IS NOT NULL`).Scan(&definitionTable); err != nil {
		t.Fatal(err)
	}
	if !definitionTable {
		t.Fatal("deployment_definitions table was not created")
	}
	var obsoleteProjectionTables int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM unnest($1::text[]) AS table_name
		 WHERE to_regclass('public.' || table_name) IS NOT NULL
	`, []string{
		"deployment_tasks",
		"deployment_sandboxes",
		"deployment_queues",
		"deployment_streams",
		"tasks",
		"sessions",
		"session_runs",
		"session_continuation_requests",
		"run_operations",
		"run_state_snapshots",
		"waits",
		"task_schedules",
		"task_schedule_instances",
		"streams",
		"stream_records",
		"workspace_process_stream_chunks",
		"workspace_process_stream_receipts",
		"workspace_process_operations",
		"actor_channels",
		"actor_inputs",
		"actor_outputs",
		"workspace_handoffs",
		"definitions",
		"queues",
		"deployment_queues",
		"schedule_definitions",
		"schedule_revisions",
		"schedule_fires",
		"schedule_leases",
		"secret_grants",
		"secret_histories",
		"outbox_attempts",
		"batches",
		"batch_members",
		"wait_groups",
	}).Scan(&obsoleteProjectionTables); err != nil {
		t.Fatal(err)
	}
	if obsoleteProjectionTables != 0 {
		t.Fatalf("obsolete deployment projection tables = %d, want 0", obsoleteProjectionTables)
	}
	requiredExecutionTables := []string{
		"deployment_definitions",
		"lookup_hmac_versions",
		"idempotency_claims",
		"workspaces",
		"workspace_versions",
		"workspace_mounts",
		"workspace_leases",
		"workspace_processes",
		"workspace_process_records",
		"secrets",
		"secret_versions",
		"workspace_secrets",
		"secret_resolutions",
		"actors",
		"actor_records",
		"runs",
		"run_attempts",
		"run_leases",
		"run_waits",
		"run_checkpoints",
		"run_checkpoint_artifacts",
		"tokens",
		"run_streams",
		"run_stream_records",
		"schedules",
		"outbox_messages",
	}
	var executionTableCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM unnest($1::text[]) AS table_name
		 WHERE to_regclass('public.' || table_name) IS NOT NULL
	`, requiredExecutionTables).Scan(&executionTableCount); err != nil {
		t.Fatal(err)
	}
	if executionTableCount != len(requiredExecutionTables) {
		t.Fatalf("greenfield execution tables = %d, want %d", executionTableCount, len(requiredExecutionTables))
	}
	var exactPinColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND (
		       (table_name = 'workspaces' AND column_name = ANY(ARRAY[
		           'deployment_definition_id', 'declaration_kind', 'workspace_declared_id'
		       ]))
		       OR
		       (table_name = 'actors' AND column_name = ANY(ARRAY[
		           'deployment_definition_id', 'declaration_kind', 'actor_declared_id'
		       ]))
		       OR
		       (table_name = 'runs' AND column_name = ANY(ARRAY[
		           'deployment_definition_id', 'entrypoint_kind', 'entrypoint_declared_id'
		       ]))
		   )
	`).Scan(&exactPinColumns); err != nil {
		t.Fatal(err)
	}
	if exactPinColumns != 9 {
		t.Fatalf("exact deployed-declaration pin columns = %d, want 9", exactPinColumns)
	}
	var exactPinIndexes int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_indexes
		 WHERE schemaname = 'public'
		   AND indexname = ANY($1::text[])
	`, []string{
		"workspaces_deployment_definition_idx",
		"actors_deployment_definition_idx",
		"runs_deployment_definition_idx",
		"run_streams_deployment_definition_idx",
	}).Scan(&exactPinIndexes); err != nil {
		t.Fatal(err)
	}
	if exactPinIndexes != 4 {
		t.Fatalf("exact deployed-declaration pin indexes = %d, want 4", exactPinIndexes)
	}
	var actorExpiryIndex bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		      FROM pg_indexes
		     WHERE schemaname = 'public'
		       AND indexname = 'actors_expiry_due_idx'
		       AND indexdef LIKE '%WHERE ((state = ''open''::text) AND (current_run_id IS NULL) AND (expires_at IS NOT NULL))%'
		)
	`).Scan(&actorExpiryIndex); err != nil {
		t.Fatal(err)
	}
	if !actorExpiryIndex {
		t.Fatal("actors_expiry_due_idx must select only open Actors without an incumbent Run and with an absolute expiry")
	}
	var safeIntegerColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND data_type = 'bigint'
		   AND (
		       (table_name = 'runs' AND column_name = 'queue_concurrency_limit')
		       OR
		       (table_name = 'actors' AND column_name = 'managed_queue_concurrency_limit')
		   )
	`).Scan(&safeIntegerColumns); err != nil {
		t.Fatal(err)
	}
	if safeIntegerColumns != 2 {
		t.Fatalf("safe-integer queue limit columns = %d, want 2", safeIntegerColumns)
	}
	var safeIntegerConstraints int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_constraint
		 WHERE conrelid IN ('runs'::regclass, 'actors'::regclass)
		   AND contype = 'c'
		   AND pg_get_constraintdef(oid) LIKE '%9007199254740991%'
		   AND pg_get_constraintdef(oid) ~ '(managed_)?queue_concurrency_limit'
	`).Scan(&safeIntegerConstraints); err != nil {
		t.Fatal(err)
	}
	if safeIntegerConstraints != 2 {
		t.Fatalf("safe-integer queue limit constraints = %d, want 2", safeIntegerConstraints)
	}
	var leaseQueuePolicyColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'run_leases'
		   AND column_name = ANY(ARRAY[
		       'queue_name', 'queue_class', 'concurrency_key', 'queue_concurrency_limit'
		   ])
	`).Scan(&leaseQueuePolicyColumns); err != nil {
		t.Fatal(err)
	}
	if leaseQueuePolicyColumns != 0 {
		t.Fatalf("copied Run Lease queue policy columns = %d, want 0", leaseQueuePolicyColumns)
	}
	var programColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'deployments'
		   AND column_name = ANY($1::text[])
	`, []string{"program_code_artifact_id", "program_dependency_artifact_id", "program_runtime_digest", "program_architecture"}).Scan(&programColumns); err != nil {
		t.Fatal(err)
	}
	if programColumns != 4 {
		t.Fatalf("deployment program columns = %d, want 4", programColumns)
	}
	var obsoleteProgramColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name IN ('deployments', 'deployment_definitions')
		   AND column_name = ANY($1::text[])
	`, []string{"program_artifact_id", "program_runtime_contract_digest", "program_supported_architectures", "runtime_contract_digest"}).Scan(&obsoleteProgramColumns); err != nil {
		t.Fatal(err)
	}
	if obsoleteProgramColumns != 0 {
		t.Fatalf("obsolete deployment program columns = %d, want 0", obsoleteProgramColumns)
	}
	var artifactKinds int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_enum
		  JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
		 WHERE pg_type.typname = 'artifact_kind'
		   AND pg_enum.enumlabel = ANY($1::text[])
	`, []string{
		"deployment_program_code",
		"deployment_program_dependencies",
		"workspace_image",
		"workspace_process_record",
	}).Scan(&artifactKinds); err != nil {
		t.Fatal(err)
	}
	if artifactKinds != 4 {
		t.Fatalf("new artifact kind labels = %d, want 4", artifactKinds)
	}
	var obsoleteArtifactKind bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_enum
			  JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
			 WHERE pg_type.typname = 'artifact_kind'
			   AND pg_enum.enumlabel = 'deployment_program'
		)
	`).Scan(&obsoleteArtifactKind); err != nil {
		t.Fatal(err)
	}
	if obsoleteArtifactKind {
		t.Fatal("obsolete deployment_program artifact kind exists")
	}
	var artifactReferenceIndexes int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_indexes
		 WHERE schemaname = 'public'
		   AND indexname = ANY($1::text[])
	`, []string{"deployments_program_code_artifact_idx", "deployments_program_dependency_artifact_idx", "deployment_definitions_artifact_idx"}).Scan(&artifactReferenceIndexes); err != nil {
		t.Fatal(err)
	}
	if artifactReferenceIndexes != 3 {
		t.Fatalf("artifact reference indexes = %d, want 3", artifactReferenceIndexes)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ('definition-region', 'test', 'definition-region', 'Definition Region');
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ('00000000-0000-0000-0000-000000001000', 'org_' || repeat('a', 26), 'Definition Org', 'definition-org');
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ('00000000-0000-0000-0000-000000002000', 'prj_' || repeat('b', 26), '00000000-0000-0000-0000-000000001000', 'definition-region', 'definition-project', 'Definition Project');
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex) VALUES
		('00000000-0000-0000-0000-000000003001', 'env_' || repeat('c', 26), '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', 'definition-one', 'Definition One', '#112233'),
		('00000000-0000-0000-0000-000000003002', 'env_' || repeat('d', 26), '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', 'definition-two', 'Definition Two', '#445566');
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type) VALUES
		('00000000-0000-0000-0000-000000001000', 'sha256:definition-source-one', 1, 'application/x-tar'),
		('00000000-0000-0000-0000-000000001000', 'sha256:definition-source-two', 1, 'application/x-tar'),
		('00000000-0000-0000-0000-000000001000', 'sha256:definition-program-code', 1, 'application/vnd.helmr.deployment-program-code.v0+squashfs'),
		('00000000-0000-0000-0000-000000001000', 'sha256:definition-program-dependencies', 1, 'application/vnd.helmr.deployment-program-dependencies.v0+squashfs'),
		('00000000-0000-0000-0000-000000001000', 'sha256:definition-workspace-one', 1, 'application/vnd.helmr.workspace-image.v0.oci-tar'),
		('00000000-0000-0000-0000-000000001000', 'sha256:definition-workspace-two', 1, 'application/vnd.helmr.workspace-image.v0.oci-tar');
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type) VALUES
		('00000000-0000-0000-0000-000000004001', '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003001', 'sha256:definition-source-one', 'deployment_source', 1, 'application/x-tar'),
		('00000000-0000-0000-0000-000000004002', '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003002', 'sha256:definition-source-two', 'deployment_source', 1, 'application/x-tar'),
		('00000000-0000-0000-0000-000000004003', '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003001', 'sha256:definition-program-code', 'deployment_program_code', 1, 'application/vnd.helmr.deployment-program-code.v0+squashfs'),
		('00000000-0000-0000-0000-000000004004', '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003001', 'sha256:definition-workspace-one', 'workspace_image', 1, 'application/vnd.helmr.workspace-image.v0.oci-tar'),
		('00000000-0000-0000-0000-000000004005', '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003002', 'sha256:definition-workspace-two', 'workspace_image', 1, 'application/vnd.helmr.workspace-image.v0.oci-tar'),
		('00000000-0000-0000-0000-000000004006', '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003001', 'sha256:definition-program-dependencies', 'deployment_program_dependencies', 1, 'application/vnd.helmr.deployment-program-dependencies.v0+squashfs');
		INSERT INTO deployments (
		    id, public_id, org_id, project_id, environment_id, build_region_id,
		    build_architecture, build_runtime_digest, build_standard_toolchain_digest,
		    build_contract_version, version, content_hash, deployment_source_artifact_id
		) VALUES
		('00000000-0000-0000-0000-000000005001', 'dep_' || repeat('e', 26), '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003001', 'definition-region', 'x86_64', decode(repeat('01', 32), 'hex'), decode(repeat('11', 32), 'hex'), 'helmr.program-build.v0', 'definition-one', 'sha256:' || repeat('1', 64), '00000000-0000-0000-0000-000000004001'),
		('00000000-0000-0000-0000-000000005002', 'dep_' || repeat('f', 26), '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003002', 'definition-region', 'x86_64', decode(repeat('01', 32), 'hex'), decode(repeat('11', 32), 'hex'), 'helmr.program-build.v0', 'definition-two', 'sha256:' || repeat('2', 64), '00000000-0000-0000-0000-000000004002'),
		('00000000-0000-0000-0000-000000005003', 'dep_' || repeat('a', 26), '00000000-0000-0000-0000-000000001000', '00000000-0000-0000-0000-000000002000', '00000000-0000-0000-0000-000000003001', 'definition-region', 'x86_64', decode(repeat('02', 32), 'hex'), decode(repeat('11', 32), 'hex'), 'helmr.program-build.v0', 'definition-one-runtime-two', 'sha256:' || repeat('1', 64), '00000000-0000-0000-0000-000000004001');
	`); err != nil {
		t.Fatal(err)
	}

	maxRegionID := strings.Repeat("r", 255)
	if _, err := tx.Exec(ctx, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'test', 'max-width-region', 'Max Width Region')
	`, maxRegionID); err != nil {
		t.Fatalf("insert maximum-width region: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO deployments (
		    id, public_id, org_id, project_id, environment_id, build_region_id,
		    build_architecture, build_runtime_digest, build_standard_toolchain_digest,
		    build_contract_version, version, content_hash,
		    api_version, sdk_version, cli_version, deployment_source_artifact_id
		) VALUES (
		    '00000000-0000-0000-0000-000000005005',
		    'dep_' || repeat('c', 26),
		    '00000000-0000-0000-0000-000000001000',
		    '00000000-0000-0000-0000-000000002000',
		    '00000000-0000-0000-0000-000000003001',
		    $1,
		    'x86_64',
		    decode(repeat('03', 32), 'hex'),
		    decode(repeat('13', 32), 'hex'),
		    'helmr.program-build.v0',
		    'max-width-reuse-key',
		    'sha256:' || repeat('3', 64),
		    repeat('a', 255),
		    repeat('b', 255),
		    repeat('c', 255),
		    '00000000-0000-0000-0000-000000004001'
		)
	`, maxRegionID); err != nil {
		t.Fatalf("insert maximum-width deployment reuse tuple: %v", err)
	}
	for _, invalidRegionID := range []string{
		strings.Repeat("r", 256),
		" padded",
		"\u00a0padded",
		"control\x01region",
		"control\u0085region",
	} {
		assertStatementRejected(t, ctx, tx, `
			INSERT INTO regions (id, provider, provider_region, display_name)
			VALUES ($1, 'test', $1, 'Invalid Region')
		`, invalidRegionID)
	}
	for column, value := range map[string]string{
		"api_version":  strings.Repeat("a", 256),
		"sdk_version":  strings.Repeat("s", 256),
		"cli_version":  strings.Repeat("c", 256),
		"content_hash": "sha256:invalid",
	} {
		assertStatementRejected(t, ctx, tx, fmt.Sprintf(`
			UPDATE deployments SET %s = $1
			 WHERE id = '00000000-0000-0000-0000-000000005005'
		`, column), value)
	}

	assertStatementRejected(t, ctx, tx, `
		INSERT INTO deployments (
		    id, public_id, org_id, project_id, environment_id, build_region_id,
		    build_architecture, build_runtime_digest, build_standard_toolchain_digest,
		    build_contract_version, version, content_hash,
		    deployment_source_artifact_id
		) VALUES (
		    '00000000-0000-0000-0000-000000005004',
		    'dep_' || repeat('b', 26),
		    '00000000-0000-0000-0000-000000001000',
		    '00000000-0000-0000-0000-000000002000',
		    '00000000-0000-0000-0000-000000003001',
		    'definition-region',
		    'x86_64',
		    decode(repeat('01', 32), 'hex'),
		    decode(repeat('11', 32), 'hex'),
		    'helmr.program-build.v0',
		    'definition-one-duplicate',
		    'sha256:' || repeat('1', 64),
		    '00000000-0000-0000-0000-000000004001'
		)
	`)

	assertStatementRejected(t, ctx, tx, `
		UPDATE deployments
		   SET program_code_artifact_id = '00000000-0000-0000-0000-000000004003'
		 WHERE id = '00000000-0000-0000-0000-000000005001'
	`)
	assertStatementRejected(t, ctx, tx, `
		UPDATE deployments
		   SET program_code_artifact_id = '00000000-0000-0000-0000-000000004003',
		       program_dependency_artifact_id = '00000000-0000-0000-0000-000000004005',
		       program_runtime_digest = decode(repeat('01', 32), 'hex'),
		       program_architecture = 'x86_64'
		 WHERE id = '00000000-0000-0000-0000-000000005001'
	`)
	if _, err := tx.Exec(ctx, `
		UPDATE deployments
		   SET program_code_artifact_id = '00000000-0000-0000-0000-000000004003',
		       program_dependency_artifact_id = '00000000-0000-0000-0000-000000004006',
		       program_runtime_digest = decode(repeat('01', 32), 'hex'),
		       program_architecture = 'x86_64'
		 WHERE id = '00000000-0000-0000-0000-000000005001'
	`); err != nil {
		t.Fatal(err)
	}
	assertStatementRejected(t, ctx, tx, `
		UPDATE deployments
		   SET program_architecture = 'amd64'
		 WHERE id = '00000000-0000-0000-0000-000000005001'
	`)
	assertStatementRejected(t, ctx, tx, `
		UPDATE deployments
		   SET program_runtime_digest = decode(repeat('01', 31), 'hex')
		 WHERE id = '00000000-0000-0000-0000-000000005001'
	`)

	if _, err := tx.Exec(ctx, `
		INSERT INTO deployment_definitions (id, environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest) VALUES
		('00000000-0000-0000-0000-000000006001', '00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'task', 'constructor', 0, '{}'::jsonb, decode(repeat('02', 32), 'hex')),
		('00000000-0000-0000-0000-000000006002', '00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'actor', 'constructor', 0, '{}'::jsonb, decode(repeat('03', 32), 'hex')),
		('00000000-0000-0000-0000-000000006003', '00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'run_stream', 'Build-', 0, '{}'::jsonb, decode(repeat('04', 32), 'hex'));
		INSERT INTO deployment_definitions (id, environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest, workspace_architecture, artifact_id)
		VALUES ('00000000-0000-0000-0000-000000006004', '00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'workspace', 'Repository.Workspace', 0, '{}'::jsonb, decode(repeat('05', 32), 'hex'), 'x86_64', '00000000-0000-0000-0000-000000004004');
	`); err != nil {
		t.Fatal(err)
	}
	assertStatementRejected(t, ctx, tx, `
		INSERT INTO deployment_definitions (environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest)
		VALUES ('00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'task', 'constructor', 0, '{}'::jsonb, decode(repeat('06', 32), 'hex'))
	`)
	for _, invalidID := range []string{" invalid", "invalid/name", "_invalid", "café", strings.Repeat("a", 129)} {
		assertStatementRejected(t, ctx, tx, `
			INSERT INTO deployment_definitions (environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest)
			VALUES ('00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'task', $1, 0, '{}'::jsonb, decode(repeat('07', 32), 'hex'))
		`, invalidID)
	}
	assertStatementRejected(t, ctx, tx, `
		INSERT INTO deployment_definitions (environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest, workspace_architecture, artifact_id)
		VALUES ('00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'workspace', 'partial-workspace', 0, '{}'::jsonb, decode(repeat('08', 32), 'hex'), 'x86_64', NULL)
	`)
	assertStatementRejected(t, ctx, tx, `
		INSERT INTO deployment_definitions (environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest, workspace_architecture, artifact_id)
		VALUES ('00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'workspace', 'cross-environment', 0, '{}'::jsonb, decode(repeat('09', 32), 'hex'), 'x86_64', '00000000-0000-0000-0000-000000004005')
	`)
	assertStatementRejected(t, ctx, tx, `
		INSERT INTO deployment_definitions (environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest)
		VALUES ('00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'task', 'array-manifest', 0, '[]'::jsonb, decode(repeat('0a', 32), 'hex'))
	`)
	assertStatementRejected(t, ctx, tx, `
		INSERT INTO deployment_definitions (environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest)
		VALUES ('00000000-0000-0000-0000-000000003001', '00000000-0000-0000-0000-000000005001', 'task', 'unsupported-manifest-version', 1, '{}'::jsonb, decode(repeat('0b', 32), 'hex'))
	`)

	var workspaceOnlyProgramCodeArtifactID *string
	if err := tx.QueryRow(ctx, `
		SELECT program_code_artifact_id::text
		  FROM deployments
		 WHERE id = '00000000-0000-0000-0000-000000005002'
	`).Scan(&workspaceOnlyProgramCodeArtifactID); err != nil {
		t.Fatal(err)
	}
	if workspaceOnlyProgramCodeArtifactID != nil {
		t.Fatalf("workspace-only deployment program code artifact = %q, want null", *workspaceOnlyProgramCodeArtifactID)
	}
}

func assertStatementRejected(t *testing.T, ctx context.Context, tx pgx.Tx, query string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, `SAVEPOINT expected_rejection`); err != nil {
		t.Fatal(err)
	}
	_, statementErr := tx.Exec(ctx, query, args...)
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT expected_rejection`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT expected_rejection`); err != nil {
		t.Fatal(err)
	}
	if statementErr == nil {
		t.Fatal("statement unexpectedly satisfied authority constraints")
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
	if err := pool.QueryRow(ctx, `
		SELECT per_vm_cpu_millis >= 2000
		       AND per_vm_memory_bytes >= 2147483648
		       AND per_vm_scratch_bytes >= 8589934592,
		       per_vm_cpu_millis >= 2001
		  FROM worker_instances
		 WHERE id = '00000000-0000-0000-0000-000000000099'
	`).Scan(&exactFit, &overShape); err != nil {
		t.Fatal(err)
	}
	if !exactFit || overShape {
		t.Fatalf("fixed build guest exact/over shape fence = %t/%t", exactFit, overShape)
	}
	logicalTables := []string{"idempotency_claims", "schedules", "workspaces", "actors", "actor_records", "runs", "run_attempts", "run_streams", "run_stream_records", "run_waits", "run_checkpoints", "run_checkpoint_artifacts", "meter_events", "telemetry_outbox"}
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
		"runs":              {"dispatch_generation", "dispatch_attempt", "dispatch_message_id", "dispatch_lease_id", "workspace_mount_id", "worker_instance_id", "execution_status", "terminal_outcome", "schedule_instance_id", "queue_class", "queue_timestamp"},
		"runtime_instances": {"runtime_epoch", "state", "instance_token", "last_heartbeat_at", "owner_run_id", "owner_run_wait_id", "workspace_mount_id", "workspace_version_id", "reserved_workspace_id"},
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
		"runtime_instances_reserved_run_uidx",
		"runtime_instances_reserved_process_uidx",
		"runtime_instances_restore_checkpoint_idx",
		"network_slots_runtime_active_uidx",
		"workspace_mounts_workspace_active_uidx",
		"workspace_mounts_runtime_active_uidx",
		"workspace_leases_workspace_active_uidx",
		"workspace_processes_workspace_active_uidx",
		"run_waits_active_run_uidx",
	}
	var indexCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = ANY($1::text[])`, requiredIndexes).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != len(requiredIndexes) {
		t.Fatalf("required managed-worker indexes = %d, want %d", indexCount, len(requiredIndexes))
	}

	var placementColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'runtime_instances'
		   AND column_name = ANY($1::text[])
	`, []string{
		"workspace_id",
		"program_deployment_id",
		"restore_checkpoint_id",
		"reserved_run_id",
		"reserved_attempt_number",
		"reserved_process_id",
		"reserved_workspace_version_id",
		"reservation_expires_at",
	}).Scan(&placementColumns); err != nil {
		t.Fatal(err)
	}
	if placementColumns != 8 {
		t.Fatalf("runtime placement columns = %d, want 8", placementColumns)
	}

	var obsoleteLeaseColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND (
		       (table_name = 'run_leases' AND column_name = 'resource_snapshot')
		       OR
		       (table_name = 'workspace_processes' AND column_name = ANY(ARRAY[
		           'instance_lease_id', 'write_lease_id', 'idempotency_key',
		           'idempotency_expires_at', 'request_fingerprint'
		       ]))
		   )
	`).Scan(&obsoleteLeaseColumns); err != nil {
		t.Fatal(err)
	}
	if obsoleteLeaseColumns != 0 {
		t.Fatalf("obsolete placement columns = %d, want 0", obsoleteLeaseColumns)
	}

	var processStates []string
	if err := pool.QueryRow(ctx, `
		SELECT array_agg(enumlabel ORDER BY enumsortorder)
		  FROM pg_enum
		  JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
		 WHERE pg_type.typname = 'workspace_process_state'
	`).Scan(&processStates); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(processStates, ","), "pending,starting,running,exit_requested,exited,cancelled,lost,failed"; got != want {
		t.Fatalf("workspace process states = %q, want %q", got, want)
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
		       'run_log_hot_chunks'
		   )
		   AND column_name = 'expires_at'
		   AND is_nullable = 'NO'
	`).Scan(&boundedHotTables); err != nil {
		t.Fatal(err)
	}
	if boundedHotTables != 0 {
		t.Fatalf("separate telemetry hot payload tables = %d, want 0", boundedHotTables)
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
	var recordColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'workspace_process_records'
		   AND column_name = ANY($1::text[])
	`, []string{
		"process_id",
		"direction",
		"stream",
		"offset_start",
		"offset_end",
		"data",
		"artifact_id",
		"content_digest",
		"size_bytes",
		"payload_expires_at",
		"payload_collected_at",
	}).Scan(&recordColumns); err != nil {
		t.Fatal(err)
	}
	if recordColumns != 11 {
		t.Fatalf("workspace process record columns = %d, want 11", recordColumns)
	}
	var hasSequence bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM information_schema.columns
			 WHERE table_schema = 'public'
			   AND table_name = 'workspace_process_records'
			   AND column_name = 'sequence'
		)
	`).Scan(&hasSequence); err != nil {
		t.Fatal(err)
	}
	if hasSequence {
		t.Fatal("workspace process records must use byte offsets, not sequence numbers")
	}
	var offsetReceiptKey bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_indexes
			 WHERE schemaname = 'public'
			   AND tablename = 'workspace_process_records'
			   AND indexdef LIKE '%(process_id, stream, offset_start)%'
			   AND indexdef LIKE 'CREATE UNIQUE INDEX%'
		)
	`).Scan(&offsetReceiptKey); err != nil {
		t.Fatal(err)
	}
	if !offsetReceiptKey {
		t.Fatal("workspace process records must retain the stream offset replay key")
	}
	var obsoleteTables int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM unnest($1::text[]) AS table_name
		 WHERE to_regclass('public.' || table_name) IS NOT NULL
	`, []string{
		"workspace_process_stream_chunks",
		"workspace_process_stream_receipts",
		"workspace_process_operations",
	}).Scan(&obsoleteTables); err != nil {
		t.Fatal(err)
	}
	if obsoleteTables != 0 {
		t.Fatalf("obsolete workspace process tables = %d, want 0", obsoleteTables)
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
