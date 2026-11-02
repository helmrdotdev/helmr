package schema

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const templatePlacementVersion = 6

func TestTemplatePlacementMigration(t *testing.T) {
	source, err := iofs.New(FS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := source.Prev(templatePlacementVersion)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	database := dbtest.Open(t)
	ctx := t.Context()
	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(previous) }); err != nil {
		t.Fatal(err)
	}
	pool := database.Pool
	// A definition and seed-only preparation spec recorded before Templates
	// named a placement.
	stored := seedOnlySpecWithoutPlacement(t)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO regions (id, display_name) VALUES ('test', 'Test');
		INSERT INTO organizations (id, name, slug)
		VALUES ('00000000-0000-0000-0000-000000000001', 'Organization', 'organization');
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ('00000000-0000-0000-0000-000000000002',
		        '00000000-0000-0000-0000-000000000001', 'test', 'project', 'Project');
		INSERT INTO environments (
			id, org_id, project_id, slug, name, color_hex,
			admission_tokens, admission_refilled_at
		) VALUES (
			'00000000-0000-0000-0000-000000000003',
			'00000000-0000-0000-0000-000000000001',
			'00000000-0000-0000-0000-000000000002', 'environment', 'Environment', '#3366ff', 1000000, clock_timestamp()
		);
		INSERT INTO deployments (environment_id, id, bundle_digest)
		SELECT id, '00000000-0000-0000-0000-000000000004', 'sha256:' || repeat('1', 64) FROM environments;
		INSERT INTO image_specs (environment_id, id, spec_digest, spec, seed)
		SELECT id, '00000000-0000-0000-0000-000000000005', 'sha256:' || repeat('2', 64), $1::jsonb, '{}'
		FROM environments;
		INSERT INTO image_specs (environment_id, id, spec_digest, spec, seed)
		SELECT id, '00000000-0000-0000-0000-000000000008', 'sha256:' || repeat('9', 64), '{"apiVersion":"other"}', '{}'
		FROM environments;
		INSERT INTO templates (environment_id, id, key)
		SELECT id, '00000000-0000-0000-0000-000000000006', 'existing' FROM environments;
		INSERT INTO template_definitions (
			environment_id, template_id, deployment_id, image_spec_id, cpu_millis, memory_bytes,
			network_policy, prepare_network_policy
		)
		SELECT id, '00000000-0000-0000-0000-000000000006', '00000000-0000-0000-0000-000000000004',
		       '00000000-0000-0000-0000-000000000005', 1000, 536870912, '{"kind":"ordinary"}', '{"kind":"block"}'
		FROM environments;
	`, pgx.QueryExecModeSimpleProtocol, string(stored))
	if err := Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	var placement string
	var spec, other []byte
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT placement FROM template_definitions),
		(SELECT spec FROM image_specs WHERE id='00000000-0000-0000-0000-000000000005'),
		(SELECT spec FROM image_specs WHERE id='00000000-0000-0000-0000-000000000008')`).Scan(&placement, &spec, &other); err != nil || placement != "default" {
		t.Fatalf("upgraded placement = %q, %v", placement, err)
	}
	if parsed, err := artifact.ParseComputerPreparationSpec(spec); err != nil || parsed.Template.Placement != definition.DefaultPlacement {
		t.Fatalf("upgraded seed-only spec: %v", err)
	}
	if string(other) != `{"apiVersion": "other"}` {
		t.Fatalf("upgrade changed a spec without a Template: %s", other)
	}
	// New definitions always state their placement, and only as a definition ID.
	dbtest.MustExec(t, ctx, pool, `INSERT INTO deployments (environment_id, id, bundle_digest)
		SELECT id, '00000000-0000-0000-0000-000000000007', 'sha256:' || repeat('3', 64) FROM environments`)
	for statement, code := range map[string]string{
		`UPDATE template_definitions SET placement = NULL`:  "23502",
		`UPDATE template_definitions SET placement = ''`:    "23514",
		`UPDATE template_definitions SET placement = 'a/b'`: "23514",
		`INSERT INTO template_definitions (environment_id, template_id, deployment_id, image_spec_id, cpu_millis, memory_bytes, network_policy, prepare_network_policy)
		 SELECT environment_id, template_id, '00000000-0000-0000-0000-000000000007', image_spec_id, cpu_millis, memory_bytes, network_policy, prepare_network_policy FROM template_definitions`: "23502",
	} {
		var pgErr *pgconn.PgError
		if _, err := pool.Exec(ctx, statement); !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s: got %v, want %s", statement, err, code)
		}
	}
	dbtest.MustExec(t, ctx, pool, `UPDATE template_definitions SET placement = 'ios-lab.v2'`)

	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(previous) }); err != nil {
		t.Fatal(err)
	}
	var columns int
	var restored bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='template_definitions' AND column_name='placement'),
		(SELECT spec = $1::jsonb FROM image_specs WHERE id='00000000-0000-0000-0000-000000000005')`, string(stored)).Scan(&columns, &restored); err != nil || columns != 0 || !restored {
		t.Fatalf("downgraded columns = %d, spec restored %t: %v", columns, restored, err)
	}
}

// seedOnlySpecWithoutPlacement returns a seed-only preparation spec as stored
// before Templates named a placement.
func seedOnlySpecWithoutPlacement(t *testing.T) []byte {
	t.Helper()
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		if spec.Template == nil {
			continue
		}
		raw, err := artifact.CanonicalComputerPreparationSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		delete(value["template"].(map[string]any), "placement")
		stored, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	t.Fatal("fixture has no seed-only Template")
	return nil
}
