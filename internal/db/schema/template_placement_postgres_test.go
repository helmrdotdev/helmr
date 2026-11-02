package schema

import (
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
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
	// A definition recorded before Templates named a placement.
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
		SELECT id, '00000000-0000-0000-0000-000000000005', 'sha256:' || repeat('2', 64), '{}', '{}'
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
	`)
	if err := Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	var placement string
	if err := pool.QueryRow(ctx, `SELECT placement FROM template_definitions`).Scan(&placement); err != nil || placement != "default" {
		t.Fatalf("upgraded placement = %q, %v", placement, err)
	}
	// New definitions always state their placement, and only as a definition ID.
	for _, statement := range []string{
		`UPDATE template_definitions SET placement = NULL`,
		`UPDATE template_definitions SET placement = ''`,
		`UPDATE template_definitions SET placement = 'a/b'`,
		`INSERT INTO template_definitions (environment_id, template_id, deployment_id, image_spec_id, cpu_millis, memory_bytes, network_policy, prepare_network_policy)
		 SELECT environment_id, template_id, '00000000-0000-0000-0000-000000000007', image_spec_id, cpu_millis, memory_bytes, network_policy, prepare_network_policy FROM template_definitions`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Fatalf("accepted %s", statement)
		}
	}
	dbtest.MustExec(t, ctx, pool, `UPDATE template_definitions SET placement = 'ios-lab.v2'`)

	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(previous) }); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='template_definitions' AND column_name='placement'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("downgraded columns = %d, %v", columns, err)
	}
}
