package schema

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUpWithPostgres(t *testing.T) {
	database := dbtest.Open(t)
	testUpWithPostgres(t, t.Context(), database.DSN, true)
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
	assertWorkspaceExecSchema(t, dbctx, pool)
	assertTelemetrySchema(t, dbctx, pool)
	assertWorkerSchema(t, dbctx, pool)
	assertDeploymentDefinitionAuthority(t, dbctx, pool)
	assertWorkspaceVersionAuthority(t, dbctx, pool)
	assertArtifactCreatorAuthority(t, dbctx, pool)
	assertIdempotencyClaimCollectionIndexes(t, dbctx, pool)
	assertExecutionAttachmentConstraints(t, dbctx, pool)
	assertRunWaitWorkspaceSuccession(t, dbctx, pool)
	assertPrimitiveLifecycleSchema(t, dbctx, pool)
	assertNoBusinessDatabaseLogic(t, dbctx, pool)
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
	assertArtifactCreatorAuthority(t, dbctx, pool)
	assertIdempotencyClaimCollectionIndexes(t, dbctx, pool)
	assertExecutionAttachmentConstraints(t, dbctx, pool)
	assertRunWaitWorkspaceSuccession(t, dbctx, pool)
	assertPrimitiveLifecycleSchema(t, dbctx, pool)
	assertNoBusinessDatabaseLogic(t, dbctx, pool)
}

func assertPrimitiveLifecycleSchema(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	t.Helper()
	lifecycleTypes := []string{
		"region_state",
		"worker_group_state",
		"telemetry_outbox_state",
		"deletion_job_status",
		"device_code_status",
		"worker_instance_state",
		"public_access_token_state",
		"token_state",
		"wait_state",
		"run_wait_state",
		"run_checkpoint_state",
		"run_status",
		"run_lease_state",
		"runtime_desired_state",
		"runtime_observed_state",
		"workspace_state",
		"workspace_desired_state",
		"workspace_dirty_state",
		"workspace_version_state",
		"workspace_mount_state",
		"workspace_lease_state",
		"workspace_process_state",
	}
	var enumTypeCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_type
		 WHERE typname = ANY($1::text[])
	`, lifecycleTypes).Scan(&enumTypeCount); err != nil {
		t.Fatal(err)
	}
	if enumTypeCount != 0 {
		t.Fatalf("Postgres lifecycle enum types = %d, want 0", enumTypeCount)
	}

	tableNames := []string{
		"worker_groups",
		"telemetry_outbox",
		"deletion_jobs",
		"device_codes",
		"worker_instances",
		"public_access_tokens",
		"tokens",
		"run_waits",
		"run_waits",
		"run_checkpoints",
		"runs",
		"run_leases",
		"runtime_instances",
		"runtime_instances",
		"workspaces",
		"workspaces",
		"workspaces",
		"workspace_versions",
		"workspace_mounts",
		"workspace_leases",
		"workspace_processes",
		"workspace_processes",
	}
	columnNames := []string{
		"state",
		"state",
		"status",
		"status",
		"state",
		"state",
		"state",
		"condition_state",
		"suspension_state",
		"state",
		"status",
		"state",
		"desired_state",
		"observed_state",
		"state",
		"desired_state",
		"dirty_state",
		"state",
		"state",
		"state",
		"state",
		"restore_desired_state",
	}
	var constrainedTextColumns int
	if err := pool.QueryRow(ctx, `
		WITH targets AS (
		    SELECT table_name, column_name
		      FROM unnest($1::text[], $2::text[]) AS target(table_name, column_name)
		)
		SELECT count(*)
		  FROM targets
		  JOIN information_schema.columns AS columns
		    ON columns.table_schema = 'public'
		   AND columns.table_name = targets.table_name
		   AND columns.column_name = targets.column_name
		   AND columns.data_type = 'text'
		 WHERE EXISTS (
		       SELECT 1
		         FROM pg_constraint
		         JOIN pg_class ON pg_class.oid = pg_constraint.conrelid
		         JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
		         JOIN pg_attribute
		           ON pg_attribute.attrelid = pg_class.oid
		          AND pg_attribute.attnum = ANY(pg_constraint.conkey)
		        WHERE pg_namespace.nspname = 'public'
		          AND pg_class.relname = targets.table_name
		          AND pg_attribute.attname = targets.column_name
		          AND pg_constraint.contype = 'c'
		 )
	`, tableNames, columnNames).Scan(&constrainedTextColumns); err != nil {
		t.Fatal(err)
	}
	if constrainedTextColumns != len(tableNames) {
		t.Fatalf(
			"constrained lifecycle TEXT columns = %d, want %d",
			constrainedTextColumns,
			len(tableNames),
		)
	}

	var uuidPrimaryKeyDefaults int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		  JOIN information_schema.table_constraints
		    ON table_constraints.table_schema = columns.table_schema
		   AND table_constraints.table_name = columns.table_name
		   AND table_constraints.constraint_type = 'PRIMARY KEY'
		  JOIN information_schema.key_column_usage
		    ON key_column_usage.constraint_schema = table_constraints.constraint_schema
		   AND key_column_usage.constraint_name = table_constraints.constraint_name
		   AND key_column_usage.table_schema = columns.table_schema
		   AND key_column_usage.table_name = columns.table_name
		   AND key_column_usage.column_name = columns.column_name
		 WHERE columns.table_schema = 'public'
		   AND columns.data_type = 'uuid'
		   AND columns.column_default IS NOT NULL
	`).Scan(&uuidPrimaryKeyDefaults); err != nil {
		t.Fatal(err)
	}
	if uuidPrimaryKeyDefaults != 0 {
		t.Fatalf("UUID primary-key defaults = %d, want 0", uuidPrimaryKeyDefaults)
	}

	var categoricalEnums int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_type
		 WHERE typname = ANY(ARRAY[
		     'wait_kind',
		     'artifact_kind',
		     'workspace_version_kind'
		 ])
	`).Scan(&categoricalEnums); err != nil {
		t.Fatal(err)
	}
	if categoricalEnums != 3 {
		t.Fatalf("categorical enum sentinels = %d, want 3", categoricalEnums)
	}

	queryFiles, err := filepath.Glob("../query/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, queryFile := range queryFiles {
		body, err := os.ReadFile(queryFile)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ToLower(body), []byte("uuidv7(")) {
			t.Fatalf("query file %s calls uuidv7()", queryFile)
		}
	}
}

func assertNoBusinessDatabaseLogic(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	t.Helper()
	var triggerCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_trigger
		  JOIN pg_class ON pg_class.oid = pg_trigger.tgrelid
		  JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
		 WHERE pg_namespace.nspname = 'public'
		   AND NOT pg_trigger.tgisinternal
	`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 0 {
		t.Fatalf("application-owned PostgreSQL triggers = %d, want 0", triggerCount)
	}

	var functionCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_proc
		  JOIN pg_namespace ON pg_namespace.oid = pg_proc.pronamespace
		 WHERE pg_namespace.nspname = 'public'
		   AND pg_proc.prokind IN ('f', 'p')
		   AND NOT EXISTS (
		       SELECT 1
		         FROM pg_depend
		        WHERE pg_depend.classid = 'pg_proc'::regclass
		          AND pg_depend.objid = pg_proc.oid
		          AND pg_depend.deptype = 'e'
		   )
	`).Scan(&functionCount); err != nil {
		t.Fatal(err)
	}
	if functionCount != 0 {
		t.Fatalf("application-owned PostgreSQL functions = %d, want 0", functionCount)
	}

	var viewCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_class
		  JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
		 WHERE pg_namespace.nspname = 'public'
		   AND pg_class.relkind IN ('v', 'm')
	`).Scan(&viewCount); err != nil {
		t.Fatal(err)
	}
	if viewCount != 0 {
		t.Fatalf("application-owned PostgreSQL views = %d, want 0", viewCount)
	}

	var ruleCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_rules
		 WHERE schemaname = 'public'
	`).Scan(&ruleCount); err != nil {
		t.Fatal(err)
	}
	if ruleCount != 0 {
		t.Fatalf("application-owned PostgreSQL rules = %d, want 0", ruleCount)
	}

	var generatedColumnCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND is_generated = 'ALWAYS'
	`).Scan(&generatedColumnCount); err != nil {
		t.Fatal(err)
	}
	if generatedColumnCount != 0 {
		t.Fatalf("application-owned generated columns = %d, want 0", generatedColumnCount)
	}

	migration, err := os.ReadFile(filepath.Join("migrations", "000001_initial.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	renderedJSONSizeConstraint := regexp.MustCompile(
		`(?s)octet_length\([^)]*::text\)`,
	)
	if renderedJSONSizeConstraint.Match(migration) {
		t.Fatal("baseline migration sizes JSON through PostgreSQL text rendering")
	}
}

func assertArtifactCreatorAuthority(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && err != pgx.ErrTxClosed {
			t.Fatal(err)
		}
	}()
	if _, err := tx.Exec(ctx, `
INSERT INTO regions (
    id, display_name
) VALUES (
    'artifact-test-region', 'Artifact test'
);
INSERT INTO organizations (
    id, name, slug
) VALUES (
    '00000000-0000-7000-8000-000000000901',
    'Artifact test',
    'artifact-test'
);
INSERT INTO projects (
    id, org_id, default_region_id, slug, name
) VALUES (
    '00000000-0000-7000-8000-000000000902',
    '00000000-0000-7000-8000-000000000901',
    'artifact-test-region',
    'artifact-test',
    'Artifact test'
);
INSERT INTO environments (
    id, org_id, project_id, slug, name, color_hex
) VALUES (
    '00000000-0000-7000-8000-000000000903',
    '00000000-0000-7000-8000-000000000901',
    '00000000-0000-7000-8000-000000000902',
    'artifact-test',
    'Artifact test',
    '#000000'
);
INSERT INTO worker_group_tokens (
    id, token_hash
) VALUES (
    '00000000-0000-7000-8000-000000000906',
    decode(repeat('06', 32), 'hex')
);
INSERT INTO worker_groups (
    id, token_id, region_id, name
) VALUES (
    'artifact-test-workers',
    '00000000-0000-7000-8000-000000000906',
    'artifact-test-region',
    'Artifact test'
);
INSERT INTO worker_pools (
    id, worker_group_id, name
) VALUES (
    '00000000-0000-7000-8000-000000000907',
    'artifact-test-workers',
    'artifact-test-pool'
);
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, worker_pool_id
) VALUES (
    '00000000-0000-7000-8000-000000000904',
    'artifact-test-worker',
    'artifact-test-workers',
    '00000000-0000-7000-8000-000000000907'
);
INSERT INTO cas_objects (
    org_id, digest, size_bytes, media_type
) VALUES (
    '00000000-0000-7000-8000-000000000901',
    'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    1,
    'application/octet-stream'
);
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind,
    size_bytes, media_type, created_by_worker_instance_id
) VALUES (
    '00000000-0000-7000-8000-000000000905',
    '00000000-0000-7000-8000-000000000901',
    '00000000-0000-7000-8000-000000000902',
    '00000000-0000-7000-8000-000000000903',
    'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    'deployment_program',
    1,
    'application/vnd.helmr.program.v0+squashfs',
    '00000000-0000-7000-8000-000000000904'
);
`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT delete_artifact_creator"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM worker_instances
 WHERE id = '00000000-0000-7000-8000-000000000904'
`); err == nil {
		t.Fatal("worker deletion removed immutable Artifact creator authority")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT delete_artifact_creator"); err != nil {
		t.Fatal(err)
	}
	var creatorID string
	if err := tx.QueryRow(ctx, `
SELECT created_by_worker_instance_id::text
  FROM artifacts
 WHERE id = '00000000-0000-7000-8000-000000000905'
`).Scan(&creatorID); err != nil {
		t.Fatal(err)
	}
	if creatorID != "00000000-0000-7000-8000-000000000904" {
		t.Fatalf("Artifact creator = %q", creatorID)
	}
}

func assertExecutionAttachmentConstraints(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	constraints := map[string]string{
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
}

func assertRunWaitWorkspaceSuccession(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	var definition string
	if err := pool.QueryRow(ctx, `
SELECT indexdef
  FROM pg_indexes
 WHERE schemaname = 'public'
   AND indexname = 'run_waits_same_workspace_child_active_uidx'
`).Scan(&definition); err != nil {
		t.Fatalf("read same-Workspace child index: %v", err)
	}
	if !strings.Contains(definition, "CREATE UNIQUE INDEX") {
		t.Fatalf("same-Workspace child index is not unique: %q", definition)
	}

	retiredRunWaitColumns := []string{
		"handoff_resume_checkpoint_id",
		"handoff_runtime_instance_id",
		"handoff_workspace_mount_id",
		"handoff_mount_generation",
	}
	var retiredColumnCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM information_schema.columns
 WHERE table_schema = 'public'
   AND table_name = 'run_waits'
   AND column_name = ANY($1::text[])
`, retiredRunWaitColumns).Scan(&retiredColumnCount); err != nil {
		t.Fatal(err)
	}
	if retiredColumnCount != 0 {
		t.Fatalf("retained-runtime Run Wait columns = %d, want none", retiredColumnCount)
	}

	var checkpointKindColumnCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM information_schema.columns
 WHERE table_schema = 'public'
   AND table_name = 'run_checkpoints'
   AND column_name = 'kind'
`).Scan(&checkpointKindColumnCount); err != nil {
		t.Fatal(err)
	}
	if checkpointKindColumnCount != 0 {
		t.Fatal("run_checkpoints.kind survived the single-source greenfield schema")
	}

	var checkpointKindTypeCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM pg_type
 WHERE typname = 'run_checkpoint_kind'
`).Scan(&checkpointKindTypeCount); err != nil {
		t.Fatal(err)
	}
	if checkpointKindTypeCount != 0 {
		t.Fatal("run_checkpoint_kind survived the single-source greenfield schema")
	}

	var successionConstraint bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
      FROM pg_constraint
     WHERE conrelid = 'run_waits'::regclass
       AND contype = 'c'
	       AND pg_get_constraintdef(oid) LIKE '%resume_workspace_version_id IS NOT NULL%'
       AND pg_get_constraintdef(oid) LIKE '%resume_writer_generation IS NOT NULL%'
)
`).Scan(&successionConstraint); err != nil {
		t.Fatal(err)
	}
	if !successionConstraint {
		t.Fatal("run_waits does not enforce exact Workspace version and writer succession")
	}
}

func assertIdempotencyClaimCollectionIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	required := []string{
		"idempotency_claims_live_expiry_idx",
		"idempotency_claims_retired_idx",
		"runs_claim_idx",
		"session_records_claim_idx",
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
		"vm_runtime_contract",
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
		"vm_runtime_contract",
		"guestd_abi",
		"adapter_abi",
	}).Scan(&runtimeProjectionColumns); err != nil {
		t.Fatal(err)
	}
	if runtimeProjectionColumns != 0 {
		t.Fatalf("runtime instance copied profile fields = %d, want 0", runtimeProjectionColumns)
	}
}

func assertDeploymentDefinitionAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var deploymentColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'deployments'
		   AND column_name = ANY($1::text[])
	`, []string{
		"bundle_digest",
		"runtime_artifact_digest",
		"program_artifact_id",
		"program_index_digest",
		"queue_config",
	}).Scan(&deploymentColumns); err != nil {
		t.Fatal(err)
	}
	if deploymentColumns != 5 {
		t.Fatalf("deployment authority columns = %d, want 5", deploymentColumns)
	}
	var bundleUniqueness int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_constraint
		 WHERE conrelid = 'deployments'::regclass
		   AND contype = 'u'
		   AND pg_get_constraintdef(oid) = 'UNIQUE (environment_id, bundle_digest)'
	`).Scan(&bundleUniqueness); err != nil {
		t.Fatal(err)
	}
	if bundleUniqueness != 1 {
		t.Fatalf("deployment bundle uniqueness constraints = %d, want 1", bundleUniqueness)
	}
	var definitionKinds int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_constraint
		 WHERE conrelid = 'deployment_definitions'::regclass
		   AND contype = 'c'
		   AND pg_get_constraintdef(oid) LIKE '%task%actor%sandbox%'
	`).Scan(&definitionKinds); err != nil {
		t.Fatal(err)
	}
	if definitionKinds != 1 {
		t.Fatalf("deployment definition kind constraint = %d, want 1", definitionKinds)
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
		[]string{"per_vm_cpu_millis", "per_vm_memory_bytes", "per_vm_guest_ephemeral_disk_bytes"}).Scan(&shapeColumns); err != nil {
		t.Fatal(err)
	}
	if shapeColumns != 3 {
		t.Fatalf("per-VM shape columns = %d, want 3", shapeColumns)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO regions (id, display_name) VALUES ('shape-region', 'Shape Region');
		INSERT INTO worker_group_tokens (id, token_hash)
		VALUES ('00000000-0000-7000-8000-000000000097', decode(repeat('07', 32), 'hex'));
		INSERT INTO worker_groups (id, token_id, region_id, name)
		VALUES ('shape-test', '00000000-0000-7000-8000-000000000097', 'shape-region', 'shape-test');
		INSERT INTO worker_pools (id, worker_group_id, name)
		VALUES ('00000000-0000-7000-8000-000000000098', 'shape-test', 'shape-pool');
		INSERT INTO worker_instances (id, resource_id, worker_group_id, worker_pool_id, per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes)
		VALUES ('00000000-0000-0000-0000-000000000099', 'shape-test', 'shape-test', '00000000-0000-7000-8000-000000000098', 2000, 2147483648, 8589934592);
	`); err != nil {
		t.Fatal(err)
	}
	var exactFit, overShape bool
	if err := pool.QueryRow(ctx, `
		SELECT per_vm_cpu_millis >= 2000
		       AND per_vm_memory_bytes >= 2147483648
		       AND per_vm_guest_ephemeral_disk_bytes >= 8589934592,
		       per_vm_cpu_millis >= 2001
		  FROM worker_instances
		 WHERE id = '00000000-0000-0000-0000-000000000099'
	`).Scan(&exactFit, &overShape); err != nil {
		t.Fatal(err)
	}
	if !exactFit || overShape {
		t.Fatalf("fixed guest exact/over shape fence = %t/%t", exactFit, overShape)
	}
	logicalTables := []string{"idempotency_claims", "schedules", "workspaces", "sessions", "session_records", "runs", "run_attempts", "run_waits", "run_checkpoints", "run_checkpoint_artifacts", "telemetry_outbox"}
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
		"run_leases_run_active_uidx",
		"run_leases_runtime_active_uidx",
		"runtime_instances_workspace_active_uidx",
		"runtime_instances_reserved_run_uidx",
		"runtime_instances_reserved_process_uidx",
		"runtime_instances_restore_checkpoint_idx",
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

}

func assertTelemetrySchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var meterTables int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.tables
		 WHERE table_schema = 'public'
		   AND table_name = 'meter_events'
	`).Scan(&meterTables); err != nil {
		t.Fatal(err)
	}
	if meterTables != 0 {
		t.Fatalf("meter_events tables = %d, want 0", meterTables)
	}

	var streamKinds []string
	rows, err := pool.Query(ctx, `
		SELECT enumlabel
		  FROM pg_enum
		  JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
		 WHERE pg_type.typname = 'telemetry_stream_kind'
		 ORDER BY enumlabel
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatal(err)
		}
		streamKinds = append(streamKinds, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(streamKinds, ",") != "event,run_log" {
		t.Fatalf("telemetry_stream_kind = %v, want [event run_log]", streamKinds)
	}

	var removedColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'telemetry_outbox'
		   AND column_name = ANY($1::text[])
	`, []string{
		"meter_event_id",
		"workspace_id",
		"resource_kind",
		"resource_id",
		"offset_start",
		"offset_end",
		"last_error",
	}).Scan(&removedColumns); err != nil {
		t.Fatal(err)
	}
	if removedColumns != 0 {
		t.Fatalf("removed telemetry_outbox columns still present = %d", removedColumns)
	}

	var sinkErrorColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'telemetry_outbox'
		   AND column_name = ANY($1::text[])
	`, []string{"ingest_error", "publish_error"}).Scan(&sinkErrorColumns); err != nil {
		t.Fatal(err)
	}
	if sinkErrorColumns != 2 {
		t.Fatalf("telemetry_outbox sink error columns = %d, want 2", sinkErrorColumns)
	}

	var ingestReadyIndexes int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_indexes
		 WHERE schemaname = 'public'
		   AND tablename = 'telemetry_outbox'
		   AND indexname = 'telemetry_outbox_ingest_ready_idx'
	`).Scan(&ingestReadyIndexes); err != nil {
		t.Fatal(err)
	}
	if ingestReadyIndexes != 0 {
		t.Fatalf("telemetry_outbox ingest-ready indexes = %d, want 0", ingestReadyIndexes)
	}

	var publishReadyDef string
	if err := pool.QueryRow(ctx, `
		SELECT indexdef
		  FROM pg_indexes
		 WHERE schemaname = 'public'
		   AND tablename = 'telemetry_outbox'
		   AND indexname = 'telemetry_outbox_publish_ready_idx'
	`).Scan(&publishReadyDef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(publishReadyDef, "stream_kind = 'event'") ||
		strings.Contains(publishReadyDef, "terminal_output") ||
		strings.Contains(publishReadyDef, "dead_lettered") {
		t.Fatalf("publish-ready index = %q", publishReadyDef)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO telemetry_outbox (
			org_id, stream_kind, source_kind, source_id, project_id, environment_id,
			deployment_id, kind, state
		) VALUES (
			'00000000-0000-4000-8000-000000000001',
			'event',
			'deployment',
			'00000000-0000-4000-8000-000000000001',
			'00000000-0000-4000-8000-000000000001',
			'00000000-0000-4000-8000-000000000001',
			'00000000-0000-4000-8000-000000000001',
			'x',
			'dead_lettered'
		)
	`)
	if err == nil {
		t.Fatal("dead_lettered telemetry_outbox state was accepted")
	}
	if !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("dead_lettered insert error = %v, want check constraint", err)
	}
}

func assertWorkspaceExecSchema(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	t.Helper()
	var payloadColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'workspace_processes'
		   AND column_name = ANY($1::text[])
	`, []string{"request", "stdin", "stdout", "stderr"}).Scan(&payloadColumns); err != nil {
		t.Fatal(err)
	}
	if payloadColumns != 4 {
		t.Fatalf("Workspace BasicExec payload columns = %d, want 4", payloadColumns)
	}
	var claimRequired bool
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable = 'NO'
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'workspace_processes'
		   AND column_name = 'claim_id'
	`).Scan(&claimRequired); err != nil {
		t.Fatal(err)
	}
	if !claimRequired {
		t.Fatal("Workspace BasicExec claim_id is nullable")
	}
}
