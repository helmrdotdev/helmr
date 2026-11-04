package schema

import (
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

const workerGroupOwnershipVersion = 5

func TestWorkerGroupOwnershipMigration(t *testing.T) {
	source, err := iofs.New(FS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := source.Prev(workerGroupOwnershipVersion)
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
	// An existing deployment group with a host that already ran a build.
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
		INSERT INTO image_specs (environment_id, id, spec_digest, spec, seed)
		SELECT id, '00000000-0000-0000-0000-000000000005', 'sha256:' || repeat('2', 64), '{}', '{}'
		FROM environments;
		INSERT INTO worker_group_tokens (id, token_hash)
		VALUES ('00000000-0000-0000-0000-000000000010', decode(repeat('07', 32), 'hex'));
		INSERT INTO worker_groups (id, token_id, region_id, name)
		VALUES ('00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000010', 'test', 'default');
		INSERT INTO worker_pools (id, worker_group_id, name)
		VALUES ('00000000-0000-0000-0000-000000000012', '00000000-0000-0000-0000-000000000011', 'pool');
		INSERT INTO worker_hosts (id, resource_id, worker_group_id, worker_pool_id, per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes)
		VALUES ('00000000-0000-0000-0000-000000000013', 'host', '00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000012', 2000, 2147483648, 8589934592);
		INSERT INTO vm_platforms (
			id, descriptor_digest, firecracker_digest, firecracker_version, snapshot_format_version,
			host_kernel_release, kernel_digest, initramfs_digest, rootfs_digest
		) VALUES (
			'sha256:' || repeat('3', 64), 'sha256:' || repeat('4', 64), 'sha256:' || repeat('5', 64), '1.12.0', '1.0.0',
			'6.12.0', 'sha256:' || repeat('6', 64), 'sha256:' || repeat('7', 64), 'sha256:' || repeat('8', 64)
		);
		INSERT INTO image_builds (
			environment_id, id, image_spec_id, idempotency_key, status, error_code, deadline_at,
			executor_epoch, worker_host_id, worker_epoch, allocation_id, channel_credential_digest,
			reserved_memory_bytes, reserved_scratch_bytes, vm_platform_id, vm_vcpu_count, cpu_config_digest,
			fenced_at, fence_evidence
		)
		SELECT id, '00000000-0000-0000-0000-000000000014', '00000000-0000-0000-0000-000000000005', 'existing', 'failed', 'executor_lost', clock_timestamp(),
		       1, '00000000-0000-0000-0000-000000000013', 1, '00000000-0000-0000-0000-000000000015', decode(repeat('09', 32), 'hex'),
		       536870912, 1073741824, 'sha256:' || repeat('3', 64), 1, 'sha256:' || repeat('a', 64),
		       clock_timestamp(), 'stopped'
		FROM environments;
		INSERT INTO image_builds (environment_id, id, image_spec_id, idempotency_key, deadline_at)
		SELECT id, '00000000-0000-0000-0000-000000000016', '00000000-0000-0000-0000-000000000005', 'queued', clock_timestamp()
		FROM environments;
		INSERT INTO deployments (environment_id, id, bundle_digest)
		SELECT id, '00000000-0000-0000-0000-000000000004', 'sha256:' || repeat('1', 64) FROM environments;
		INSERT INTO boot_seed_builds (
			environment_id, id, deployment_id, image_spec_id, cpu_millis, memory_bytes, status, error_code,
			deadline_at, executor_epoch, worker_host_id, worker_epoch, allocation_id, resource_policy_revision,
			channel_credential_digest, reserved_memory_bytes, reserved_scratch_bytes, vm_platform_id, vm_vcpu_count,
			cpu_config_digest, fenced_at, fence_evidence
		)
		SELECT id, '00000000-0000-0000-0000-000000000017', '00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000005',
		       1000, 536870912, 'failed', 'executor_stopped',
		       clock_timestamp(), 1, '00000000-0000-0000-0000-000000000013', 1, '00000000-0000-0000-0000-000000000018', 1,
		       decode(repeat('0a', 32), 'hex'), 536870912, 1073741824, 'sha256:' || repeat('3', 64), 1,
		       'sha256:' || repeat('a', 64), clock_timestamp(), 'stopped'
		FROM environments;
	`)
	if err := Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	var deploymentGroup, recorded, queuedUnassigned bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT owner_org_id IS NULL FROM worker_groups WHERE id='00000000-0000-0000-0000-000000000011'),
		(SELECT worker_group_id='00000000-0000-0000-0000-000000000011' FROM image_builds WHERE id='00000000-0000-0000-0000-000000000014')
		AND (SELECT worker_group_id='00000000-0000-0000-0000-000000000011' FROM boot_seed_builds WHERE id='00000000-0000-0000-0000-000000000017'),
		(SELECT worker_group_id IS NULL FROM image_builds WHERE id='00000000-0000-0000-0000-000000000016')`).Scan(&deploymentGroup, &recorded, &queuedUnassigned); err != nil {
		t.Fatal(err)
	}
	if !deploymentGroup || !recorded || !queuedUnassigned {
		t.Fatalf("upgraded rows: deployment group %t, producing group %t, unassigned build %t", deploymentGroup, recorded, queuedUnassigned)
	}

	// Downgrading refuses to drop organization groups and succeeds without them.
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_group_tokens (id, token_hash)
		VALUES ('00000000-0000-0000-0000-000000000020', decode(repeat('08', 32), 'hex'));
		INSERT INTO worker_groups (id, owner_org_id, token_id, region_id, name)
		VALUES ('00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000020', 'test', 'default');
	`)
	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(previous) }); err == nil {
		t.Fatal("downgrade dropped an organization group")
	}
	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Force(workerGroupOwnershipVersion) }); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, pool, `DELETE FROM worker_groups WHERE owner_org_id IS NOT NULL`)
	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(previous) }); err != nil {
		t.Fatal(err)
	}
	var owner int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND column_name IN ('owner_org_id','worker_group_id','pin_org_id','pin_project_id','pin_region_id') AND table_name IN ('worker_groups','computers','image_builds','boot_seed_builds')`).Scan(&owner); err != nil || owner != 0 {
		t.Fatalf("downgraded columns = %d, %v", owner, err)
	}
}
