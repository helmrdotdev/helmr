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

	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
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
	pool, err := openPool(dbctx, dsn)
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
	assertDeploymentDefinitionAuthority(t, dbctx, pool)
	assertComputerDiskVersionAuthority(t, dbctx, pool)
	assertArtifactCreatorAuthority(t, dbctx, pool)
	assertIdempotencyClaimCollectionIndexes(t, dbctx, pool)
	assertExecutionAttachmentConstraints(t, dbctx, pool)
	assertCheckpointMembershipAuthority(t, dbctx, pool)
	assertPrimitiveLifecycleSchema(t, dbctx, pool)
	assertNoRedundantGlobalIDUniqueness(t, dbctx, pool)
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
	assertComputerCommandSchema(t, dbctx, pool)
	assertTelemetrySchema(t, dbctx, pool)
	assertWorkerSchema(t, dbctx, pool)
	assertDeploymentDefinitionAuthority(t, dbctx, pool)
	assertComputerDiskVersionAuthority(t, dbctx, pool)
	assertArtifactCreatorAuthority(t, dbctx, pool)
	assertIdempotencyClaimCollectionIndexes(t, dbctx, pool)
	assertExecutionAttachmentConstraints(t, dbctx, pool)
	assertCheckpointMembershipAuthority(t, dbctx, pool)
	assertPrimitiveLifecycleSchema(t, dbctx, pool)
	assertNoRedundantGlobalIDUniqueness(t, dbctx, pool)
	assertNoBusinessDatabaseLogic(t, dbctx, pool)
}

func assertNoRedundantGlobalIDUniqueness(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT redundant.conrelid::regclass::text, redundant.conname
  FROM pg_constraint AS redundant
  JOIN pg_attribute AS id_column
    ON id_column.attrelid = redundant.conrelid
   AND id_column.attname = 'id'
   AND NOT id_column.attisdropped
 WHERE redundant.contype = 'u'
   AND cardinality(redundant.conkey) > 1
   AND id_column.attnum = ANY(redundant.conkey)
   AND EXISTS (
       SELECT 1
         FROM pg_constraint AS primary_key
        WHERE primary_key.conrelid = redundant.conrelid
          AND primary_key.contype = 'p'
          AND primary_key.conkey = ARRAY[id_column.attnum]::smallint[]
   )
   AND NOT EXISTS (
       SELECT 1
         FROM pg_constraint AS foreign_key
        WHERE foreign_key.contype = 'f'
          AND foreign_key.confrelid = redundant.conrelid
          AND foreign_key.confkey @> redundant.conkey
          AND foreign_key.confkey <@ redundant.conkey
   )
 ORDER BY 1, 2
`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, constraint string
		if err := rows.Scan(&table, &constraint); err != nil {
			t.Fatal(err)
		}
		violations = append(violations, table+"."+constraint)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("global-ID-implied UNIQUE constraints without an exact FK consumer: %v", violations)
	}

	var apiKeyIndexIsNonUnique bool
	var apiKeyIndexDefinition string
	if err := pool.QueryRow(ctx, `
SELECT NOT pg_index.indisunique, pg_get_indexdef(pg_index.indexrelid)
  FROM pg_index
 WHERE pg_index.indexrelid = 'api_keys_scope_created_idx'::regclass
`).Scan(&apiKeyIndexIsNonUnique, &apiKeyIndexDefinition); err != nil {
		t.Fatal(err)
	}
	if !apiKeyIndexIsNonUnique {
		t.Fatal("api_keys_scope_created_idx is unique")
	}
	if !strings.HasSuffix(
		apiKeyIndexDefinition,
		"USING btree (org_id, project_id, environment_id, created_at DESC, id DESC)",
	) {
		t.Fatalf("api_keys_scope_created_idx definition = %q", apiKeyIndexDefinition)
	}
}

func assertPrimitiveLifecycleSchema(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	t.Helper()
	tableNames := []string{"secrets", "worker_pools", "idempotency_claims", "schedules", "sessions", "control_outbox", "worker_groups", "telemetry_outbox", "device_codes", "worker_hosts", "public_access_tokens", "tokens", "run_waits", "run_waits", "computer_checkpoints", "runs", "run_leases", "computer_instances", "computer_instances", "computer_instances", "computer_instances", "computers", "computers", "computers", "computer_disk_versions", "computer_commands"}
	columnNames := []string{"status", "status", "status", "status", "status", "status", "status", "status", "status", "status", "status", "status", "condition_status", "suspension_status", "status", "status", "status", "desired_state", "observed_state", "admission_state", "mount_state", "status", "desired_state", "dirty_state", "status", "status"}
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
		     'artifact_kind'
		 ])
	`).Scan(&categoricalEnums); err != nil {
		t.Fatal(err)
	}
	if categoricalEnums != 2 {
		t.Fatalf("categorical enum sentinels = %d, want 2", categoricalEnums)
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

	// Generated columns project physical byte accounting and FK availability
	// keys and conditional retention references. Lifecycle transitions and metadata
	// admission remain owned by Go.
	var generatedColumns []string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(c.relname || '.' || a.attname || ':' || a.attgenerated::text ORDER BY c.relname, a.attname), ARRAY[]::text[])
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND a.attgenerated <> '' AND NOT a.attisdropped
	`).Scan(&generatedColumns); err != nil {
		t.Fatal(err)
	}
	if strings.Join(generatedColumns, ",") != "cas_blobs.not_retired:s,cas_objects.availability_required:s,computer_checkpoint_objects.availability_required:s,computer_checkpoints.computer_payload_required:s,computer_commands.outcome_kind:s,computer_data_keys.available:s,computer_disk_version_roots.certification_required:s,computer_disk_version_roots.direct_key_required:s,computer_disk_version_roots.logical_bytes:s,computer_disk_version_roots.payload_required:s,computer_disk_version_roots.root_kind:s,computer_disk_version_roots.root_pack_digest:s,computer_disk_version_roots.root_pack_rank:s,computer_disk_version_roots.root_pack_size_bytes:s,computer_disk_version_roots.root_page_key_id:s,computer_disk_versions.payload_not_retired:s,computer_instances.computer_key_available:s,computer_instances.computer_payload_required:s,computer_instances.retained_source_disk_version_id:s,computer_instances.retained_write_key_id:s,computer_instances.spec_retention_required:s,computer_object_edges.certification_required:s,computer_object_keys.availability_required:s,computer_objects.availability_required:s,computer_objects.certified:s,computer_objects.certified_org_id:s,computer_specs.seed_available:s,computer_specs.seed_kind:s,computers.computer_payload_required:s,computers.recovery_payload_required:s,computers.spec_retention_required:s,computers.write_key_available:s,run_attempts.computer_payload_required:s,run_waits.computer_payload_required:s,runs.computer_payload_required:s,telemetry_outbox.ingest_size_bytes:s" {
		t.Fatalf("unexpected generated storage columns: %v", generatedColumns)
	}

	// Deparsed CHECK definitions parenthesize column casts. Inspect admission
	// constraints rather than banning serialization accounting in all SQL.
	rows, err := pool.Query(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace='public'::regnamespace AND contype='c'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if renderedJSONSizeConstraint.MatchString(definition) {
			t.Fatalf("CHECK sizes metadata through PostgreSQL text rendering: %s", definition)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
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
    '01900000-0000-7000-8000-000000000908',
    '00000000-0000-7000-8000-000000000906',
    'artifact-test-region',
    'Artifact test'
);
INSERT INTO worker_pools (
    id, worker_group_id, name
) VALUES (
    '00000000-0000-7000-8000-000000000907',
    '01900000-0000-7000-8000-000000000908',
    'artifact-test-pool'
);
INSERT INTO worker_hosts (
    id, resource_id, worker_group_id, worker_pool_id
) VALUES (
    '00000000-0000-7000-8000-000000000904',
    'artifact-test-worker',
    '01900000-0000-7000-8000-000000000908',
    '00000000-0000-7000-8000-000000000907'
);
WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ('sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 1) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (
    org_id, digest, size_bytes, media_type
) VALUES (
    '00000000-0000-7000-8000-000000000901',
    'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    1,
    'application/vnd.helmr.program.v0+squashfs'
);
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind,
    size_bytes, media_type, created_by_worker_host_id
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
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	planRows, err := tx.Query(ctx, `
EXPLAIN (COSTS OFF)
DELETE FROM artifacts
 WHERE org_id = '00000000-0000-7000-8000-000000000901'
   AND digest = 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
`)
	if err != nil {
		t.Fatal(err)
	}
	var planLines []string
	for planRows.Next() {
		var line string
		if err := planRows.Scan(&line); err != nil {
			planRows.Close()
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err := planRows.Err(); err != nil {
		planRows.Close()
		t.Fatal(err)
	}
	planRows.Close()
	if plan := strings.Join(planLines, "\n"); !strings.Contains(plan, "artifacts_cas_scope_idx") {
		t.Fatalf("CAS child delete plan does not use artifacts_cas_scope_idx:\n%s", plan)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT mismatched_artifact_descriptor"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES (
    '00000000-0000-7000-8000-000000000908',
    '00000000-0000-7000-8000-000000000901',
    '00000000-0000-7000-8000-000000000902',
    '00000000-0000-7000-8000-000000000903',
    'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    'deployment_program', 2, 'application/vnd.helmr.program.v0+squashfs'
)
`); err == nil {
		t.Fatal("Artifact descriptor mismatch bypassed CAS authority")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT mismatched_artifact_descriptor"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT delete_artifact_creator"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM worker_hosts
 WHERE id = '00000000-0000-7000-8000-000000000904'
`); err == nil {
		t.Fatal("worker deletion removed immutable Artifact creator authority")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT delete_artifact_creator"); err != nil {
		t.Fatal(err)
	}
	var creatorID string
	if err := tx.QueryRow(ctx, `
SELECT created_by_worker_host_id::text
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
		"computer_instances_restore_checkpoint_computer_fkey": "FOREIGN KEY (source_checkpoint_id, computer_id) REFERENCES computer_checkpoints(id, computer_id)",
		"computer_instances_retained_computer_source_fkey":    "FOREIGN KEY (environment_id, computer_id, retained_source_disk_version_id) REFERENCES computer_disk_version_roots(environment_id, computer_id, version_id)",
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

func assertCheckpointMembershipAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, columns := range []string{
		"run_id, attempt_number, computer_id, run_wait_id",
		"environment_id, computer_id, checkpoint_id, source_computer_instance_id, writer_generation",
		"run_id, attempt_number, computer_id, source_run_lease_id, source_computer_instance_id, writer_generation",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint
WHERE conrelid='computer_checkpoint_runs'::regclass AND contype='f'
AND pg_get_constraintdef(oid) LIKE $1)`, "FOREIGN KEY ("+columns+")%").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("checkpoint membership does not bind %s", columns)
		}
	}
}

func assertIdempotencyClaimCollectionIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	required := []string{
		"idempotency_claims_receipt_gc_idx",
		"runs_claim_idx",
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

func assertComputerDiskVersionAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var authorityColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'computer_disk_versions'
		   AND column_name = ANY($1::text[])
	`, []string{
		"parent_version_id",
		"root_pack_digest",
		"source_computer_instance_id",
		"writer_generation",
		"published_at",
		"discarded_at",
	}).Scan(&authorityColumns); err != nil {
		t.Fatal(err)
	}
	if authorityColumns != 6 {
		t.Fatalf("computer version authority columns = %d, want 6", authorityColumns)
	}
	var oneRoot bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_indexes
			 WHERE schemaname = 'public'
			   AND tablename = 'computer_disk_versions'
			   AND indexdef LIKE 'CREATE UNIQUE INDEX%'
			   AND indexdef LIKE '%(computer_id)%'
			   AND indexdef LIKE '%WHERE (parent_version_id IS NULL)%'
		)
	`).Scan(&oneRoot); err != nil {
		t.Fatal(err)
	}
	if !oneRoot {
		t.Fatal("computer versions do not enforce one generation-zero root")
	}
	var fencedSource bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_constraint
			 WHERE conrelid = 'computer_disk_versions'::regclass
			   AND contype = 'f'
			   AND pg_get_constraintdef(oid) LIKE '%source_computer_instance_id, writer_generation%'
		)
	`).Scan(&fencedSource); err != nil {
		t.Fatal(err)
	}
	if !fencedSource {
		t.Fatal("computer versions do not bind their source instance and writer fence")
	}
	var runtimeProjectionColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'computer_instances'
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
	assertWorkerGroupUUIDSchema(t, ctx, pool)
	var shapeColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='worker_hosts' AND column_name = ANY($1::text[])`,
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
		VALUES ('01900000-0000-7000-8000-000000000096', '00000000-0000-7000-8000-000000000097', 'shape-region', 'shape-test');
		INSERT INTO worker_pools (id, worker_group_id, name)
		VALUES ('00000000-0000-7000-8000-000000000098', '01900000-0000-7000-8000-000000000096', 'shape-pool');
		INSERT INTO worker_hosts (id, resource_id, worker_group_id, worker_pool_id, per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes)
		VALUES ('00000000-0000-0000-0000-000000000099', 'shape-test', '01900000-0000-7000-8000-000000000096', '00000000-0000-7000-8000-000000000098', 2000, 2147483648, 8589934592);
	`); err != nil {
		t.Fatal(err)
	}
	var exactFit, overShape bool
	if err := pool.QueryRow(ctx, `
		SELECT per_vm_cpu_millis >= 2000
		       AND per_vm_memory_bytes >= 2147483648
		       AND per_vm_guest_ephemeral_disk_bytes >= 8589934592,
		       per_vm_cpu_millis >= 2001
		  FROM worker_hosts
		 WHERE id = '00000000-0000-0000-0000-000000000099'
	`).Scan(&exactFit, &overShape); err != nil {
		t.Fatal(err)
	}
	if !exactFit || overShape {
		t.Fatalf("fixed guest exact/over shape fence = %t/%t", exactFit, overShape)
	}
	logicalTables := []string{"idempotency_claims", "schedules", "computers", "sessions", "session_turns", "session_messages", "session_events", "runs", "run_attempts", "run_waits", "computer_checkpoints", "telemetry_outbox"}
	var assignmentLeaks int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = ANY($1::text[])
		   AND column_name = 'worker_group_id'
	`, logicalTables).Scan(&assignmentLeaks); err != nil {
		t.Fatal(err)
	}
	if assignmentLeaks != 0 {
		t.Fatalf("logical worker_group_id columns = %d, want 0", assignmentLeaks)
	}

	requiredIndexes := []string{
		"run_leases_process_cleanup_idx",
		"run_leases_run_active_uidx", "run_leases_computer_instance_active_idx",
		"computer_instances_computer_active_uidx", "computer_instances_source_checkpoint_idx",
		"computer_instances_writer_expiry_idx", "computer_commands_pending_idx",
		"computer_commands_unreconciled_idx", "run_waits_active_run_uidx",
	}
	var indexCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = ANY($1::text[])`, requiredIndexes).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != len(requiredIndexes) {
		t.Fatalf("required managed-worker indexes = %d, want %d", indexCount, len(requiredIndexes))
	}

	var checkpointArtifactColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'computer_checkpoints'
		   AND column_name = ANY($1::text[])
	`, []string{"vm_config_artifact_id", "vm_state_artifact_id", "memory_artifact_id", "scratch_disk_artifact_id"}).Scan(&checkpointArtifactColumns); err != nil {
		t.Fatal(err)
	}
	if checkpointArtifactColumns != 4 {
		t.Fatalf("fixed checkpoint artifact columns = %d, want 4", checkpointArtifactColumns)
	}
	var checkpointConstraints int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		 WHERE connamespace = 'public'::regnamespace
		   AND conname = ANY($1::text[])
	`, []string{
		"cas_objects_descriptor_key",
		"artifacts_cas_descriptor_fk",
		"computer_checkpoints_artifact_shape_check",
		"computer_checkpoints_ready_artifacts_check",
		"computer_checkpoints_vm_config_artifact_fk",
		"computer_checkpoints_vm_state_artifact_fk",
		"computer_checkpoints_memory_artifact_fk",
		"computer_checkpoints_scratch_disk_artifact_fk",
	}).Scan(&checkpointConstraints); err != nil {
		t.Fatal(err)
	}
	if checkpointConstraints != 8 {
		t.Fatalf("checkpoint descriptor constraints = %d, want 8", checkpointConstraints)
	}
	var casScopeIndexDefinition string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef('public.artifacts_cas_scope_idx'::regclass)`).Scan(&casScopeIndexDefinition); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(casScopeIndexDefinition, "USING btree (org_id, digest)") {
		t.Fatalf("artifacts_cas_scope_idx definition = %q", casScopeIndexDefinition)
	}

	var assignmentColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'computer_instances'
		   AND column_name = ANY($1::text[])
	`, []string{
		"computer_id",
		"program_deployment_id",
		"source_checkpoint_id",
		"computer_spec_id",
		"writer_generation",
		"writer_token_hash",
		"writer_expires_at",
		"membership_revision",
	}).Scan(&assignmentColumns); err != nil {
		t.Fatal(err)
	}
	if assignmentColumns != 8 {
		t.Fatalf("instance assignment columns = %d, want 8", assignmentColumns)
	}

}

func assertWorkerGroupUUIDSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tables := []string{"worker_groups", "worker_pools", "worker_hosts", "worker_host_credentials", "run_leases", "computer_instances"}
	columns := []string{"id", "worker_group_id", "worker_group_id", "worker_group_id", "worker_group_id", "worker_group_id"}
	nullability := []string{"NO", "NO", "NO", "NO", "NO", "NO"}
	rows, err := pool.Query(ctx, `
		WITH expected AS (
			SELECT *
			  FROM unnest($1::text[], $2::text[], $3::text[])
			       AS target(table_name, column_name, is_nullable)
		), actual AS (
			SELECT table_name, column_name, data_type, is_nullable
			  FROM information_schema.columns
			 WHERE table_schema = 'public'
			   AND ((table_name = 'worker_groups' AND column_name = 'id')
			        OR column_name = 'worker_group_id')
		)
		SELECT table_name, column_name,
		       COALESCE(actual.data_type, ''), COALESCE(actual.is_nullable, '')
		  FROM expected
		  FULL JOIN actual USING (table_name, column_name)
		 WHERE expected.table_name IS NULL
		    OR actual.table_name IS NULL
		    OR actual.data_type IS DISTINCT FROM 'uuid'
		    OR actual.is_nullable IS DISTINCT FROM expected.is_nullable
		 ORDER BY 1, 2
	`, tables, columns, nullability)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, column, dataType, nullable string
		if err := rows.Scan(&table, &column, &dataType, &nullable); err != nil {
			t.Fatal(err)
		}
		violations = append(violations, table+"."+column+"="+dataType+" nullable="+nullable)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("Worker Group UUID catalog violations: %v", violations)
	}
}

func assertTelemetrySchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
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
	if strings.Join(streamKinds, ",") != "command_log,event,run_log" {
		t.Fatalf("telemetry_stream_kind = %v, want [command_log event run_log]", streamKinds)
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
	if !strings.Contains(publishReadyDef, "stream_kind = 'event'") {
		t.Fatalf("publish-ready index = %q", publishReadyDef)
	}

}

func assertComputerCommandSchema(
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
		   AND table_name = 'computer_commands'
		   AND column_name = ANY($1::text[])
	`, []string{"argv", "cwd", "env", "timeout_ms", "stdin"}).Scan(&payloadColumns); err != nil {
		t.Fatal(err)
	}
	if payloadColumns != 5 {
		t.Fatalf("Command input columns = %d, want 5", payloadColumns)
	}
	var claimRequired bool
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable = 'NO'
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'computer_commands'
		   AND column_name = 'claim_id'
	`).Scan(&claimRequired); err != nil {
		t.Fatal(err)
	}
	if !claimRequired {
		t.Fatal("Computer BasicExec claim_id is nullable")
	}
}

// Covers PostgreSQL's direct column-cast deparse, e.g. octet_length((payload)::text).
var renderedJSONSizeConstraint = regexp.MustCompile(`(?s)octet_length\(\(*[a-zA-Z_][a-zA-Z_0-9]*\)*::text\)`)

func TestJSONSizeGuardMatchesPostgresDeparse(t *testing.T) {
	database := dbtest.Open(t)
	pool, err := openPool(t.Context(), database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(t.Context(), `CREATE TABLE json_size_guard_probe(payload jsonb CHECK(octet_length(payload::text)<=100))`); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := pool.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='json_size_guard_probe'::regclass AND contype='c'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !renderedJSONSizeConstraint.MatchString(definition) {
		t.Fatalf("representation-dependent admission check was missed: %s", definition)
	}
}

func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return dbpool.New(ctx, config)
}
