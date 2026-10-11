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
	if err := pool.QueryRow(dbctx, `SELECT to_regclass('public.sessions') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("sessions table was not created")
	}
	assertExecutionOwnershipSchema(t, dbctx, pool)
	assertPrimitiveLifecycleSchema(t, dbctx, pool)
	assertNoRedundantGlobalIDUniqueness(t, dbctx, pool)
	assertDatabaseConstraintBoundaries(t, dbctx, pool)
	if !verifyDown {
		return
	}
	if err := Down(dbctx, dsn); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if err := pool.QueryRow(dbctx, `SELECT to_regclass('public.sessions') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("sessions table still exists after down migration")
	}
	if err := Up(dbctx, dsn); err != nil {
		t.Fatalf("migration after down failed: %v", err)
	}
	if err := pool.QueryRow(dbctx, `SELECT to_regclass('public.sessions') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("sessions table was not recreated after down migration")
	}
	assertComputerCommandSchema(t, dbctx, pool)
	assertTelemetrySchema(t, dbctx, pool)
	assertWorkerSchema(t, dbctx, pool)
	assertExecutionOwnershipSchema(t, dbctx, pool)
	assertPrimitiveLifecycleSchema(t, dbctx, pool)
	assertNoRedundantGlobalIDUniqueness(t, dbctx, pool)
	assertDatabaseConstraintBoundaries(t, dbctx, pool)
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
	tableNames := []string{"secrets", "worker_pools", "sessions", "control_outbox", "worker_groups", "device_codes", "worker_hosts", "computer_checkpoints", "computer_leases", "computer_commands", "session_processes", "turns", "computer_saves", "computer_preparations"}
	columnNames := []string{"status", "status", "status", "status", "status", "status", "status", "status", "status", "status", "status", "status", "status", "status"}
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
		     'org_member_role',
		     'magic_link_purpose'
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

func assertDatabaseConstraintBoundaries(
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
 AND NOT (pg_trigger.tgname='platform_retry_completion' AND pg_class.relname='platform_retry_keys' AND pg_trigger.tgconstraint<>0 AND pg_trigger.tgdeferrable AND pg_trigger.tginitdeferred)
	`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 0 {
		t.Fatalf("application-owned PostgreSQL triggers = %d, want 0", triggerCount)
	}

	// The deferred retry constraint rejects incomplete transaction-local key
	// acquisitions; it does not advance lifecycle state or schedule work.
	var completionConstraint bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='platform_retry_keys'::regclass AND tgname='platform_retry_completion' AND tgconstraint<>0 AND tgdeferrable AND tginitdeferred AND tgfoid='require_platform_retry_completion()'::regprocedure)`).Scan(&completionConstraint); err != nil {
		t.Fatal(err)
	}
	if !completionConstraint {
		t.Fatal("missing deferred retry completion constraint")
	}
	var functionCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_proc
		  JOIN pg_namespace ON pg_namespace.oid = pg_proc.pronamespace
		 WHERE pg_namespace.nspname = 'public'
		   AND pg_proc.prokind IN ('f', 'p')
 AND pg_proc.proname<>'require_platform_retry_completion'
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

	var views []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(table_name ORDER BY table_name),ARRAY[]::text[]) FROM information_schema.views WHERE table_schema='public' AND is_updatable='NO'`).Scan(&views); err != nil {
		t.Fatal(err)
	}
	if strings.Join(views, ",") != "computer_secret_revocations" {
		t.Fatalf("read-only derived views=%v", views)
	}
	var otherViews int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('v','m') AND c.relname<>'computer_secret_revocations'`).Scan(&otherViews); err != nil {
		t.Fatal(err)
	}
	if otherViews != 0 {
		t.Fatalf("unexpected views=%d", otherViews)
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
	if strings.Join(generatedColumns, ",") != "cas_blobs.not_retired:s,cas_objects.availability_required:s,computer_checkpoint_objects.availability_required:s,computer_data_keys.available:s,computer_disk_roots.certification_required:s,computer_disk_roots.direct_key_required:s,computer_disk_roots.logical_bytes:s,computer_disk_roots.root_kind:s,computer_disk_roots.root_pack_digest:s,computer_disk_roots.root_pack_rank:s,computer_disk_roots.root_pack_size_bytes:s,computer_disk_roots.root_page_key_id:s,computer_disk_roots.root_page_offset:s,computer_leases.restored_published:s,computer_leases.retained_base_root_id:s,computer_leases.write_key_required:s,computer_object_edges.certification_required:s,computer_object_keys.availability_required:s,computer_objects.availability_required:s,computer_objects.certified:s,computer_objects.certified_org_id:s,computer_preparations.write_key_required:s,computer_saves.published:s,computer_saves.requires_recorded_result:s,computers.recovery_published:s,telemetry_outbox.ingest_size_bytes:s,telemetry_outbox.producer_epoch:s,telemetry_outbox.source_id:s,telemetry_outbox.source_kind:s,turns.completion_published:s,turns.has_recorded_result:s" {
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
	logicalTables := []string{"sessions", "turns", "computers", "computer_commands", "computer_checkpoints", "telemetry_outbox", "agent_schedules"}
	var assignmentLeaks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=ANY($1::text[]) AND column_name='worker_group_id'`, logicalTables).Scan(&assignmentLeaks); err != nil {
		t.Fatal(err)
	}
	if assignmentLeaks != 0 {
		t.Fatalf("logical group assignment columns=%d", assignmentLeaks)
	}
	requiredIndexes := []string{"computer_leases_one_writer", "computer_leases_expiring", "computer_leases_host_unfenced", "session_processes_one_writer", "session_processes_computer_custody", "computer_commands_pending", "computer_commands_unreconciled", "computer_checkpoints_one_pending", "turns_one_active", "turns_queue", "platform_retry_keys_receipt_expiry"}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=ANY($1::text[])`, requiredIndexes).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(requiredIndexes) {
		t.Fatalf("worker/admission indexes=%d want %d", count, len(requiredIndexes))
	}

}

func assertWorkerGroupUUIDSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tables := []string{"worker_groups", "worker_pools", "worker_hosts", "worker_host_secrets"}
	columns := []string{"id", "worker_group_id", "worker_group_id", "worker_group_id"}
	nullability := []string{"NO", "NO", "NO", "NO"}
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
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='telemetry_outbox' AND column_name=ANY($1::text[])`, []string{"session_id", "process_epoch", "preparation_id", "preparation_epoch", "command_id", "deployment_id", "source_kind", "source_id", "producer_epoch", "ingest_size_bytes"}).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 10 {
		t.Fatalf("typed telemetry owner columns=%d", columns)
	}
	var index string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND indexname='telemetry_outbox_event_publish'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(index, "stream_kind = 'event'") {
		t.Fatalf("event publish index=%s", index)
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
	assertForeignKey(t, ctx, pool, "computer_commands", "environment_id, computer_id, computer_lease_epoch", "computer_leases(environment_id, computer_id, epoch)")

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

func assertForeignKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, columns, target string) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=$1::regclass AND contype='f' AND pg_get_constraintdef(oid) LIKE $2)`, table, "FOREIGN KEY ("+columns+") REFERENCES "+target+"%").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("%s missing typed FK (%s) -> %s", table, columns, target)
	}
}
func assertExecutionOwnershipSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, fk := range [][3]string{
		{"agent_definitions", "environment_id, deployment_id, computer_definition_key", "computer_definitions(environment_id, deployment_id, definition_key)"},
		{"computer_definitions", "environment_id, preparation_spec_id", "computer_preparation_specs(environment_id, id)"},
		{"computer_saves", "environment_id, computer_id, computer_lease_epoch", "computer_leases(environment_id, computer_id, epoch)"},
		{"computer_saves", "environment_id, turn_id, computer_id, requires_recorded_result", "turns(environment_id, id, computer_id, has_recorded_result)"},
		{"turns", "environment_id, computer_id, id, completion_save_id, completion_published", "computer_saves(environment_id, computer_id, turn_id, id, published)"},
		{"computer_checkpoint_members", "environment_id, computer_id, session_id, process_epoch", "session_processes(environment_id, computer_id, session_id, epoch)"},
		{"computer_checkpoint_members", "environment_id, computer_id, checkpoint_id", "computer_checkpoints(environment_id, computer_id, id)"},
		{"computer_checkpoints", "environment_id, computer_id, disk_save_id", "computer_saves(environment_id, computer_id, id)"},
		{"computer_checkpoints", "environment_id, computer_id, source_lease_epoch", "computer_leases(environment_id, computer_id, epoch)"},
		{"computer_checkpoints", "environment_id, computer_id, target_lease_epoch", "computer_leases(environment_id, computer_id, epoch)"},
		{"computer_checkpoint_objects", "digest, size_bytes, availability_required", "cas_blobs(digest, size_bytes, not_retired)"},
	} {
		assertForeignKey(t, ctx, pool, fk[0], fk[1], fk[2])
	}
}
