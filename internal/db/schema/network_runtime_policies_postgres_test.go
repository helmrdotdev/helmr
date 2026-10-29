package schema

import (
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestNetworkRuntimePoliciesMigration(t *testing.T) {
	database := dbtest.Open(t)
	ctx := t.Context()
	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(1) }); err != nil {
		t.Fatal(err)
	}
	pool := database.Pool
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO regions (id, display_name) VALUES ('test', 'Test');
		INSERT INTO organizations (id, name, slug)
		VALUES ('00000000-0000-0000-0000-000000000001', 'Organization', 'organization');
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ('00000000-0000-0000-0000-000000000002',
		        '00000000-0000-0000-0000-000000000001', 'test', 'project', 'Project');
		INSERT INTO environments (
			id, org_id, project_id, slug, name, color_hex,
			max_outstanding_admissions, max_causal_depth, max_resident_computers,
			max_cpu_millis, max_memory_bytes, max_reserved_storage_bytes,
			preparation_timeout_ms, admission_rate_per_second, admission_burst,
			admission_tokens, admission_refilled_at
		) VALUES (
			'00000000-0000-0000-0000-000000000003',
			'00000000-0000-0000-0000-000000000001',
			'00000000-0000-0000-0000-000000000002', 'environment', 'Environment', '#3366ff',
			1024, 32, 1024, 1024000, 1099511627776, 1099511627776,
			600000, 1000000, 1000000, 1000000, clock_timestamp()
		);
		INSERT INTO deployments (environment_id, id, bundle_digest)
		SELECT id, '00000000-0000-0000-0000-000000000004', 'sha256:' || repeat('1', 64)
		FROM environments;
		INSERT INTO image_specs (environment_id, id, spec_digest, spec, seed)
		SELECT id, '00000000-0000-0000-0000-000000000005', 'sha256:' || repeat('2', 64), '{}', '{}'
		FROM environments;
		INSERT INTO templates (environment_id, id, key)
		SELECT id, '00000000-0000-0000-0000-000000000006'::uuid, 'plain' FROM environments
		UNION ALL
		SELECT id, '00000000-0000-0000-0000-000000000007'::uuid, 'prepared' FROM environments;
		INSERT INTO template_definitions (
			environment_id, template_id, deployment_id, image_spec_id, prepare, cpu_millis, memory_bytes
		)
		SELECT environment_id, id, '00000000-0000-0000-0000-000000000004',
		       '00000000-0000-0000-0000-000000000005', key = 'prepared', 1000, 1048576
		FROM templates;
		INSERT INTO agents (environment_id, id, key)
		SELECT id, '00000000-0000-0000-0000-000000000008', 'agent' FROM environments;
		INSERT INTO agent_definitions (environment_id, agent_id, deployment_id, template_id, has_init)
		SELECT environment_id, id, '00000000-0000-0000-0000-000000000004',
		       '00000000-0000-0000-0000-000000000007', true
		FROM agents;
	`)
	// These are the stored callback and seed-only shapes before policy bindings.
	const callbackSpec = `{"apiVersion":"helmr.computer-preparation.v1","program":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mediaType":"application/vnd.helmr.deployment-program.v0+squashfs","sizeBytes":4096},"templateDefinitionId":"prepared"}`
	const seedSpec = `{"apiVersion":"helmr.computer-preparation.v1","runtime":{"architecture":"x86_64","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","runtimeContract":"helmr.runtime.v0"},"template":{"buildSecrets":[],"prepare":false,"resources":{"memoryMiB":4096,"milliCpu":2000},"secrets":[],"seed":{"artifactDigest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","config":{"Cmd":null,"Entrypoint":null,"Env":null,"User":"","WorkingDir":""},"mediaType":"application/vnd.helmr.computer.seed.v0+filepack","profile":"linux-amd64-ext4-v1"}},"templateDefinitionId":"plain"}`
	dbtest.MustExec(t, ctx, pool, `UPDATE image_specs SET spec = $1, spec_digest = $2`, seedSpec, dbtest.Digest(seedSpec))
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO image_specs (environment_id, id, spec_digest, spec, seed)
		SELECT id, '00000000-0000-0000-0000-000000000009', $1, $2, '{}'
		FROM environments
	`, dbtest.Digest(callbackSpec), callbackSpec)
	dbtest.MustExec(t, ctx, pool, `
		UPDATE template_definitions SET image_spec_id = '00000000-0000-0000-0000-000000000009'
		WHERE prepare
	`)
	existingData := func() string {
		t.Helper()
		var value string
		if err := pool.QueryRow(ctx, `
			SELECT jsonb_build_object(
				'templates', (SELECT jsonb_agg(to_jsonb(d) - 'network_policy' - 'prepare_network_policy'
				                             ORDER BY template_id) FROM template_definitions d),
				'agents', (SELECT jsonb_agg(to_jsonb(d) - 'runtime_mcp_policy'
				                          ORDER BY agent_id) FROM agent_definitions d),
				'preparations', (SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM image_specs s)
			)::text
		`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	assertVersion := func(want int) {
		t.Helper()
		var version int
		var dirty bool
		if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
			t.Fatal(err)
		}
		if version != want || dirty {
			t.Fatalf("migration state = (%d, dirty=%t), want (%d, false)", version, dirty, want)
		}
	}
	assertVersion(1)
	originalData := existingData()
	originalCatalog := dumpCatalog(t, ctx, pool)
	wantCatalog, err := os.ReadFile(catalogSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	for cycle := range 2 {
		if err := Up(ctx, database.DSN); err != nil {
			t.Fatalf("upgrade %d: %v", cycle, err)
		}
		assertVersion(3)
		if got := existingData(); got != originalData {
			t.Fatalf("upgrade changed existing data: got %s, want %s", got, originalData)
		}
		if got := dumpCatalog(t, ctx, pool); got != string(wantCatalog) {
			t.Fatalf("upgraded catalog differs from %s:\n%s", catalogSnapshot, catalogDiff(string(wantCatalog), got))
		}
		var templates, agents int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM template_definitions
			WHERE network_policy = '{"kind":"ordinary"}'::jsonb
			  AND prepare_network_policy = CASE WHEN prepare
			      THEN '{"kind":"ordinary"}'::jsonb ELSE '{"kind":"block"}'::jsonb END
		`).Scan(&templates); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM agent_definitions
			WHERE runtime_mcp_policy = '{"kind":"unrestricted"}'::jsonb
		`).Scan(&agents); err != nil {
			t.Fatal(err)
		}
		if templates != 2 || agents != 1 {
			t.Fatalf("migrated policies: templates=%d agents=%d, want 2 and 1", templates, agents)
		}
		for _, id := range []string{"00000000-0000-0000-0000-000000000005", "00000000-0000-0000-0000-000000000009"} {
			var stored []byte
			if err := pool.QueryRow(ctx, `SELECT spec FROM image_specs WHERE id = $1`, id).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if _, err := artifact.ParseComputerPreparationSpec(stored); err == nil {
				t.Fatalf("accepted preparation %s without a policy binding; redeployment is required", id)
			}
		}
		if cycle == 0 {
			if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(1) }); err != nil {
				t.Fatal(err)
			}
			assertVersion(1)
			if got := dumpCatalog(t, ctx, pool); got != originalCatalog {
				t.Fatalf("rollback changed original catalog:\n%s", catalogDiff(originalCatalog, got))
			}
			if got := existingData(); got != originalData {
				t.Fatalf("rollback changed existing data: got %s, want %s", got, originalData)
			}
		}
	}
}
