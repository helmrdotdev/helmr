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
	limits := cfg.environmentExecutionLimits
	if err := limits.Validate(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, devSeedEnvironmentsSQL, limits.MaxOutstandingAdmissions, limits.MaxCausalDepth, limits.MaxResidentComputers, limits.MaxCPUMillis, limits.MaxMemoryBytes, limits.MaxReservedStorageBytes, limits.PreparationTimeoutMS, limits.AdmissionRatePerSecond, limits.AdmissionBurst); err != nil {
		return err
	}
	if err := seedDemoEnvironmentData(ctx, tx); err != nil {
		return err
	}
	if err := seedPermissionData(ctx, tx, cfg.auditPolicy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const devSeedSQL = `
INSERT INTO users (id, display_name, primary_email)
VALUES ('00000000-0000-7000-8000-000000000101', 'Local Developer', 'dev@helmr.local');

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-7000-8000-000000000201', 'Helmr Local', 'local-dev');

INSERT INTO org_members (id, org_id, user_id, is_owner, display_name)
VALUES ('00000000-0000-7000-8000-000000001101', '00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000101', true, 'Local Developer');

INSERT INTO projects (id, org_id, default_region_id, slug, name, is_default)
VALUES ('00000000-0000-7000-8000-000000000301', '00000000-0000-7000-8000-000000000201', current_setting('helmr.seed_region_id'), 'console-demo', 'Console Demo', true);
`

// devSeedEnvironmentsSQL writes the demo Environments with the configured
// execution limits; the token bucket starts full.
const devSeedEnvironmentsSQL = `
INSERT INTO environments ( id, org_id, project_id, slug, name, color_hex, is_default,
    max_outstanding_admissions, max_causal_depth, max_resident_computers, max_cpu_millis, max_memory_bytes, max_reserved_storage_bytes, preparation_timeout_ms, admission_rate_per_second, admission_burst, admission_tokens, admission_refilled_at)
SELECT  seed.id, '00000000-0000-7000-8000-000000000201', '00000000-0000-7000-8000-000000000301', seed.slug, seed.name, seed.color_hex, seed.is_default,
    $1, $2, $3, $4, $5, $6, $7, $8, $9::bigint, $9::bigint::numeric, clock_timestamp()
FROM (VALUES
    ('00000000-0000-7000-8000-000000000401'::uuid, 'production', 'Production', '#315FCE', true),
    ('00000000-0000-7000-8000-000000000402'::uuid, 'staging', 'Staging', '#F59E0B', false),
    ('00000000-0000-7000-8000-000000000403'::uuid, 'demo', 'Demo', '#9333EA', false)
) AS seed(id, slug, name, color_hex, is_default)
`
