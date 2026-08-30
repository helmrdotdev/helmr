package db_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRuntimeSubstrateRegistrationIsImmutableAndConcurrent(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)

	definitionID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest, artifact_id
		) VALUES ($1, $2, $3, 'sandbox', 'substrate-test', 0, '{}'::jsonb,
		          decode(repeat('01', 32), 'hex'), $4)
	`, definitionID, ids.environmentID, ids.deploymentID, ids.workspaceImageArtifactID)

	params := db.InsertRuntimeSubstrateParams{
		ID:                     pgvalue.UUID(uuid.NewV7()),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		DeploymentDefinitionID: pgvalue.UUID(definitionID),
		SubstrateDigest:        dbtest.Digest("substrate-first"),
		SubstrateFormat:        substrate.Format,
		SubstrateContract:      substrate.Contract,
		SubstrateSizeBytes:     4096,
	}
	rows, err := queries.InsertRuntimeSubstrate(ctx, params)
	if err != nil || rows != 1 {
		t.Fatalf("insert rows=%d error=%v", rows, err)
	}
	first := getRuntimeSubstrateRegistration(t, ctx, queries, params)

	replay := params
	replay.ID = pgvalue.UUID(uuid.NewV7())
	rows, err = queries.InsertRuntimeSubstrate(ctx, replay)
	if err != nil || rows != 0 {
		t.Fatalf("replay rows=%d error=%v", rows, err)
	}
	replayed := getRuntimeSubstrateRegistration(t, ctx, queries, replay)
	if replayed.ID != first.ID || !replayed.CreatedAt.Time.Equal(first.CreatedAt.Time) {
		t.Fatalf("replay mutated registration: first=%+v replay=%+v", first, replayed)
	}

	conflicting := replay
	conflicting.SubstrateDigest = dbtest.Digest("substrate-conflict")
	rows, err = queries.InsertRuntimeSubstrate(ctx, conflicting)
	if err != nil || rows != 0 {
		t.Fatalf("conflicting insert rows=%d error=%v", rows, err)
	}
	if _, err := queries.GetRuntimeSubstrateRegistration(ctx, registrationLookup(conflicting)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("conflicting output lookup error=%v, want no rows", err)
	}

	concurrent := params
	concurrent.ID = pgvalue.UUID(uuid.NewV7())
	concurrent.SubstrateContract = substrate.Contract + ".concurrent"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txCommitted := false
	defer func() {
		if !txCommitted {
			_ = tx.Rollback(context.Background())
		}
	}()
	if rows, err = db.New(tx).InsertRuntimeSubstrate(ctx, concurrent); err != nil || rows != 1 {
		t.Fatalf("concurrent first insert rows=%d error=%v", rows, err)
	}
	result := make(chan db.RuntimeSubstrate, 1)
	failure := make(chan error, 1)
	go func() {
		other := concurrent
		other.ID = pgvalue.UUID(uuid.NewV7())
		if inserted, insertErr := queries.InsertRuntimeSubstrate(ctx, other); insertErr != nil {
			failure <- insertErr
		} else if inserted != 0 {
			failure <- errors.New("concurrent replay inserted a second row")
		} else {
			row, lookupErr := queries.GetRuntimeSubstrateRegistration(ctx, registrationLookup(other))
			if lookupErr != nil {
				failure <- lookupErr
			} else {
				result <- row
			}
		}
	}()
	select {
	case row := <-result:
		t.Fatalf("concurrent replay returned before first commit: %+v", row)
	case err := <-failure:
		t.Fatalf("concurrent replay failed before first commit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	txCommitted = true
	select {
	case row := <-result:
		if row.ID != concurrent.ID {
			t.Fatalf("concurrent replay ID=%s, want %s", row.ID, concurrent.ID)
		}
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent replay did not resume after commit")
	}
}

func TestLockRuntimeSubstrateAuthorityFencesWorkerAndContract(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	fixture := seedRuntimeSubstrateAuthority(t, ctx, pool)
	queries := db.New(pool)
	params := db.LockRuntimeSubstrateAuthorityParams{
		DeploymentDefinitionID: pgvalue.UUID(fixture.definitionID),
		WorkerInstanceID:       pgvalue.UUID(fixture.workerID),
		WorkerGroupID:          dbtest.DefaultWorkerGroupID,
		WorkerEpoch:            1,
		SubstrateFormat:        substrate.Format,
		SubstrateContract:      substrate.Contract,
	}
	if _, err := queries.LockRuntimeSubstrateAuthority(ctx, params); err != nil {
		t.Fatal(err)
	}

	rejected := []struct {
		name   string
		mutate func(db.LockRuntimeSubstrateAuthorityParams) db.LockRuntimeSubstrateAuthorityParams
	}{
		{name: "stale epoch", mutate: func(value db.LockRuntimeSubstrateAuthorityParams) db.LockRuntimeSubstrateAuthorityParams {
			value.WorkerEpoch = 2
			return value
		}},
		{name: "another worker", mutate: func(value db.LockRuntimeSubstrateAuthorityParams) db.LockRuntimeSubstrateAuthorityParams {
			value.WorkerInstanceID = pgvalue.UUID(uuid.NewV7())
			return value
		}},
		{name: "another definition", mutate: func(value db.LockRuntimeSubstrateAuthorityParams) db.LockRuntimeSubstrateAuthorityParams {
			value.DeploymentDefinitionID = pgvalue.UUID(uuid.NewV7())
			return value
		}},
		{name: "format mismatch", mutate: func(value db.LockRuntimeSubstrateAuthorityParams) db.LockRuntimeSubstrateAuthorityParams {
			value.SubstrateFormat += ".other"
			return value
		}},
		{name: "contract mismatch", mutate: func(value db.LockRuntimeSubstrateAuthorityParams) db.LockRuntimeSubstrateAuthorityParams {
			value.SubstrateContract += ".other"
			return value
		}},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			if _, err := queries.LockRuntimeSubstrateAuthority(ctx, test.mutate(params)); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("error=%v, want no rows", err)
			}
		})
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET state = 'draining', draining_at = now()
		 WHERE id = $1
	`, fixture.workerID)
	if _, err := queries.LockRuntimeSubstrateAuthority(ctx, params); err != nil {
		t.Fatalf("draining authority: %v", err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET runtime_identity_id = NULL,
		       substrate_format = '',
		       substrate_contract = '',
		       epoch_cpu_millis = 0,
		       epoch_memory_bytes = 0,
		       epoch_guest_ephemeral_disk_bytes = 0,
		       per_vm_cpu_millis = 0,
		       per_vm_memory_bytes = 0,
		       per_vm_guest_ephemeral_disk_bytes = 0,
		       max_vm_slots = 0,
		       max_runtime_starts = 0,
		       activated_at = NULL
		 WHERE id = $1
	`, fixture.workerID)
	if _, err := queries.LockRuntimeSubstrateAuthority(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cleanup-only authority error=%v, want no rows", err)
	}
}

type runtimeSubstrateAuthorityFixture struct {
	definitionID uuid.UUID
	workerID     uuid.UUID
}

func seedRuntimeSubstrateAuthority(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}) runtimeSubstrateAuthorityFixture {
	t.Helper()
	orgID := dbtest.DefaultOrgID
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	deploymentID := uuid.NewV7()
	programArtifactID := uuid.NewV7()
	imageArtifactID := uuid.NewV7()
	bundleDigest := dbtest.Digest("authority-bundle-" + deploymentID.String())
	runtimeDigest := dbtest.Digest("authority-runtime-" + deploymentID.String())
	programDigest := dbtest.Digest("authority-program-" + deploymentID.String())
	imageDigest := dbtest.Digest("authority-image-" + deploymentID.String())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Default', 'default')
		ON CONFLICT (id) DO NOTHING
	`, orgID)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, 'Authority')
	`, projectID, orgID, dbtest.DefaultRegionID, "authority-"+dbtest.ShortID(projectID))
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Authority', '#3366ff')
	`, environmentID, orgID, projectID, "authority-"+dbtest.ShortID(environmentID))
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'), ($1, $3, 1, 'application/octet-stream')
	`, orgID, programDigest, imageDigest)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES
			($1, $3, $4, $5, $6, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($2, $3, $4, $5, $7, 'workspace_image', 1, 'application/octet-stream')
	`, programArtifactID, imageArtifactID, orgID, projectID, environmentID, programDigest, imageDigest)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO deployments (
			id, org_id, project_id, environment_id, version, bundle_digest,
			runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
		) VALUES (
			$1, $2, $3, $4, 'authority', $5, $6, $7,
			decode(repeat('03', 32), 'hex'), '{}'::jsonb
		)
	`, deploymentID, orgID, projectID, environmentID,
		bundleDigest, runtimeDigest, programArtifactID)
	definitionID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest, artifact_id
		) VALUES (
			$1, $2, $3, 'sandbox', 'authority-workspace', 0, '{}'::jsonb,
			decode(repeat('03', 32), 'hex'), $4
		)
	`, definitionID, environmentID, deploymentID, imageArtifactID)

	workspaceID := uuid.NewV7()
	rootVersionID := uuid.NewV7()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspaces (
			id, environment_id, region_id,
			sandbox_declared_id, deployment_definition_id, head_version_id
		) VALUES ($1, $2, $3, 'authority-workspace', $4, $5)
	`, workspaceID, environmentID, dbtest.DefaultRegionID, definitionID, rootVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, environment_id, workspace_id,
			kind, content_digest, state, ownership_generation, writer_generation, published_at
		) VALUES (
			$1, $2, $3, 'system',
			'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
			'committed', 0, 0, now()
		)
	`, rootVersionID, environmentID, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	workerID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, worker_pool_id, state,
			current_epoch, current_service_id,
			runtime_identity_id,
			substrate_format, substrate_contract,
			epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, max_runtime_starts,
			cpu_environment, cpu_environment_digest,
			observed_at, epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, $4, 'active',
			1, $5,
			$6, 'ext4', 'helmr.substrate.ext4.v0',
			8000, 17179869184, 274877906944,
			4000, 8589934592, 34359738368,
			8, 1, '{}'::jsonb, $7, now(), now(), now()
		)
	`, workerID, "authority-"+workerID.String(), dbtest.DefaultWorkerGroupID,
		dbtest.DefaultWorkerPoolID, uuid.NewV7(), dbtest.DefaultRuntimeID,
		dbtest.DefaultCPUConfigID)
	runtimeID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_definition_id, worker_epoch,
			vm_vcpu_count, cpu_config_digest,
			reserved_cpu_millis, reserved_memory_bytes,
			reserved_guest_ephemeral_disk_bytes,
			reserved_execution_slots, workspace_id, desired_reason
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, 1,
			1, $11,
			1000, 1073741824, 2147483648,
			1, $10, 'authority-test'
		)
	`, runtimeID, orgID, dbtest.DefaultWorkerGroupID, projectID, environmentID,
		dbtest.DefaultRegionID, workerID, dbtest.DefaultRuntimeID, definitionID,
		workspaceID, dbtest.DefaultCPUConfigID)
	return runtimeSubstrateAuthorityFixture{definitionID: definitionID, workerID: workerID}
}

func getRuntimeSubstrateRegistration(t *testing.T, ctx context.Context, queries *db.Queries, params db.InsertRuntimeSubstrateParams) db.RuntimeSubstrate {
	t.Helper()
	row, err := queries.GetRuntimeSubstrateRegistration(ctx, registrationLookup(params))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func registrationLookup(params db.InsertRuntimeSubstrateParams) db.GetRuntimeSubstrateRegistrationParams {
	return db.GetRuntimeSubstrateRegistrationParams{
		OrgID:                  params.OrgID,
		ProjectID:              params.ProjectID,
		EnvironmentID:          params.EnvironmentID,
		DeploymentDefinitionID: params.DeploymentDefinitionID,
		SubstrateDigest:        params.SubstrateDigest,
		SubstrateFormat:        params.SubstrateFormat,
		SubstrateContract:      params.SubstrateContract,
		SubstrateSizeBytes:     params.SubstrateSizeBytes,
	}
}
