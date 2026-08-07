package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedDevData(ctx context.Context, pool *pgxpool.Pool, cfg devConfig) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('helmr.seed_region_id', $1, true)`, cfg.bootstrapRegionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, devSeedSQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const devSeedSQL = `
INSERT INTO users (id, display_name, primary_email)
VALUES ('00000000-0000-7000-8000-000000000101', 'Local Developer', 'dev@helmr.local')
ON CONFLICT (id) DO UPDATE
   SET display_name = EXCLUDED.display_name,
       primary_email = EXCLUDED.primary_email,
       disabled_at = NULL,
       updated_at = now();

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-7000-8000-000000000201', 'Helmr Local', 'local-dev')
ON CONFLICT (id) DO UPDATE
   SET name = EXCLUDED.name,
       slug = EXCLUDED.slug,
       updated_at = now();

INSERT INTO org_members (org_id, user_id, role, display_name)
VALUES ('00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000101', 'owner', 'Local Developer')
ON CONFLICT (org_id, user_id) DO UPDATE
   SET role = EXCLUDED.role,
       display_name = EXCLUDED.display_name,
       disabled_at = NULL,
       updated_at = now();

INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default)
VALUES ('00000000-0000-7000-8000-000000000301', '00000000-0000-7000-8000-000000000201', current_setting('helmr.seed_region_id'), 'console-demo', 'Console Demo', true)
ON CONFLICT (id) DO UPDATE
   SET default_region_id = EXCLUDED.default_region_id,
       slug = EXCLUDED.slug,
       name = EXCLUDED.name,
       is_default = EXCLUDED.is_default,
       updated_at = now();

INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default)
VALUES
    ('00000000-0000-7000-8000-000000000401', '00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000301', 'production', 'Production', '#315FCE', true),
    ('00000000-0000-7000-8000-000000000402', '00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000301', 'staging', 'Staging', '#F59E0B', false)
ON CONFLICT (id) DO UPDATE
   SET slug = EXCLUDED.slug,
       name = EXCLUDED.name,
       color_hex = EXCLUDED.color_hex,
       is_default = EXCLUDED.is_default,
       updated_at = now();
`
