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
	if _, err := tx.Exec(ctx, `SELECT set_config('helmr.seed_region_id', $1, true)`, cfg.bootstrap.RegionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, devSeedSQL); err != nil {
		return err
	}
	if err := seedDemoEnvironmentData(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const devSeedSQL = `
INSERT INTO users (id, display_name, primary_email)
VALUES ('00000000-0000-7000-8000-000000000101', 'Local Developer', 'dev@helmr.local');

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-7000-8000-000000000201', 'Helmr Local', 'local-dev');

INSERT INTO org_members (org_id, user_id, role, display_name)
VALUES ('00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000101', 'owner', 'Local Developer');

INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default)
VALUES ('00000000-0000-7000-8000-000000000301', '00000000-0000-7000-8000-000000000201', current_setting('helmr.seed_region_id'), 'console-demo', 'Console Demo', true);

INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, is_default)
VALUES
    ('00000000-0000-7000-8000-000000000401', '00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000301', 'production', 'Production', '#315FCE', true),
    ('00000000-0000-7000-8000-000000000402', '00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000301', 'staging', 'Staging', '#F59E0B', false),
    ('00000000-0000-7000-8000-000000000403', '00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000301', 'demo', 'Demo', '#9333EA', false);
`
