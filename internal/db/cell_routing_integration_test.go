package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cell"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestActiveEnvironmentRouteAuthoritySurvivesStaleCellHealth(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := pool.Exec(ctx, `
		UPDATE cell_health
		   SET routing_fresh_until = now() - interval '1 minute'
		 WHERE cell_id = $1
	`, dbtest.DefaultCellID); err != nil {
		t.Fatal(err)
	}

	route, err := queries.GetActiveEnvironmentCellRoute(ctx, db.GetActiveEnvironmentCellRouteParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RegionID:      dbtest.DefaultRegionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.CellID != dbtest.DefaultCellID {
		t.Fatalf("route cell = %q", route.CellID)
	}
}

func TestCellHealthRequiresAllRoutingComponents(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	_ = seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	required := []string{"control", "dispatcher"}
	if _, err := pool.Exec(ctx, `
		DELETE FROM cell_component_health
		 WHERE cell_id = $1
		   AND component = 'dispatcher'
	`, dbtest.DefaultCellID); err != nil {
		t.Fatal(err)
	}

	if err := cell.ReportHealth(ctx, queries, cell.HealthConfig{
		CellID:             dbtest.DefaultCellID,
		Component:          "control",
		RequiredComponents: required,
	}); err != nil {
		t.Fatal(err)
	}
	readiness, err := queries.GetControlCellReadiness(ctx, dbtest.DefaultCellID)
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Routable {
		t.Fatalf("routable = true with missing dispatcher component")
	}

	if err := cell.ReportHealth(ctx, queries, cell.HealthConfig{
		CellID:             dbtest.DefaultCellID,
		Component:          "dispatcher",
		RequiredComponents: required,
	}); err != nil {
		t.Fatal(err)
	}
	readiness, err = queries.GetControlCellReadiness(ctx, dbtest.DefaultCellID)
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.Routable {
		t.Fatalf("routable = false after required components reported: %+v", readiness)
	}
}

func TestPrepareQueuedRunQueueItemRequiresFreshCellHealth(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	seedSessionForRun(t, ctx, pool, ids)
	runtimeID := "runtime-routing-health"
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := queries.EnsureRuntimeReleaseSelection(ctx, runtimeID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE cell_health
		   SET routing_fresh_until = now() - interval '1 minute'
		 WHERE cell_id = $1
	`, dbtest.DefaultCellID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
}

func TestScopedRunReadsUseSourceRouteGenerationWithoutHealthFreshness(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	markEnvironmentRouteDrainingWithStaleHealth(t, ctx, pool, ids)

	rows, err := queries.ListScopedRunSummaries(ctx, db.ListScopedRunSummariesParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StatusFilter:  "all",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != pgvalue.UUID(ids.runID) || rows[0].RouteGeneration != 1 {
		t.Fatalf("rows = %+v, want seeded run on route generation 1", rows)
	}

	counts, err := queries.CountScopedRunsByStatus(ctx, db.CountScopedRunsByStatusParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Waiting != 1 {
		t.Fatalf("counts = %+v, want waiting=1", counts)
	}
}

func TestScopedRunReadsRejectDisabledSourceRouteGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := pool.Exec(ctx, `
		UPDATE environment_cells
		   SET route_state = 'disabled'
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND cell_id = $4
		   AND route_generation = 1
	`, ids.orgID, ids.projectID, ids.environmentID, dbtest.DefaultCellID); err != nil {
		t.Fatal(err)
	}

	rows, err := queries.ListScopedRunSummaries(ctx, db.ListScopedRunSummariesParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StatusFilter:  "all",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for disabled source route", rows)
	}

	counts, err := queries.CountScopedRunsByStatus(ctx, db.CountScopedRunsByStatusParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Waiting != 0 {
		t.Fatalf("counts = %+v, want waiting=0 for disabled source route", counts)
	}
}

func TestWorkspaceSessionStartRejectsDisabledSourceRouteGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := queries.GetWorkspaceForSessionStart(ctx, db.GetWorkspaceForSessionStartParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatalf("workspace before disable: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE environment_cells
		   SET route_state = 'disabled'
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND cell_id = $4
		   AND route_generation = 1
	`, ids.orgID, ids.projectID, ids.environmentID, dbtest.DefaultCellID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.GetWorkspaceForSessionStart(ctx, db.GetWorkspaceForSessionStartParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("workspace after disable err = %v, want no rows", err)
	}
}

func TestCurrentDeploymentForRouteRequiresActiveRouteGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	secondCellID := "us-east-1-cell-2"
	ensureCellRoute(t, ctx, pool, ids, secondCellID, 2)
	secondDeploymentID := seedDeploymentInCell(t, ctx, pool, ids, secondCellID, 2)
	if _, err := pool.Exec(ctx, `
		UPDATE environments
		   SET current_deployment_id = $1
		 WHERE org_id = $2
		   AND project_id = $3
		   AND id = $4
	`, secondDeploymentID, ids.orgID, ids.projectID, ids.environmentID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.GetCurrentDeploymentForRoute(ctx, db.GetCurrentDeploymentForRouteParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		CellID:          dbtest.DefaultCellID,
		RouteGeneration: 1,
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old route current deployment err = %v, want pgx.ErrNoRows", err)
	}

	deployment, err := queries.GetCurrentDeploymentForRoute(ctx, db.GetCurrentDeploymentForRouteParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		CellID:          secondCellID,
		RouteGeneration: 2,
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.ID != pgvalue.UUID(secondDeploymentID) || deployment.CellID != secondCellID || deployment.RouteGeneration != 2 {
		t.Fatalf("deployment = %+v, want second-cell route generation 2", deployment)
	}
}

func TestPromoteDeploymentCarriesDeploymentRouteGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	ensureCellRoute(t, ctx, pool, ids, dbtest.DefaultCellID, 2)
	if _, err := pool.Exec(ctx, `
		UPDATE deployments
		   SET route_generation = 2
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.deploymentID); err != nil {
		t.Fatal(err)
	}

	promotion, err := queries.PromoteDeployment(ctx, db.PromoteDeploymentParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		CellID:              dbtest.DefaultCellID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		DeploymentID:        pgvalue.UUID(ids.deploymentID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PromotedByPrincipal: "test",
		Reason:              "route-generation-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if promotion.RouteGeneration != 2 {
		t.Fatalf("promotion route_generation = %d, want 2", promotion.RouteGeneration)
	}
}

func TestReusableDeploymentBuildKeyIsRouteGenerationScoped(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	contentHash := "same-content-" + shortUUID(uuid.Must(uuid.NewV7()))
	var workerGroupID uuid.UUID
	var artifactID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT worker_group_id, deployment_source_artifact_id
		  FROM deployments
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.deploymentID).Scan(&workerGroupID, &artifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:                   testDeploymentPublicID(t),
		OrgID:                      pgvalue.UUID(ids.orgID),
		CellID:                     dbtest.DefaultCellID,
		RouteGeneration:            1,
		ProjectID:                  pgvalue.UUID(ids.projectID),
		EnvironmentID:              pgvalue.UUID(ids.environmentID),
		Version:                    "queued.1",
		ApiVersion:                 "2026-06-06",
		SdkVersion:                 "",
		CliVersion:                 "",
		BundleFormatVersion:        2,
		WorkerProtocolVersion:      "helmr.worker.v1",
		WorkerGroupID:              pgvalue.UUID(workerGroupID),
		ContentHash:                contentHash,
		DeploymentSourceArtifactID: pgvalue.UUID(artifactID),
		Status:                     db.DeploymentStatusQueued,
	}); err != nil {
		t.Fatal(err)
	}
	ensureCellRoute(t, ctx, pool, ids, dbtest.DefaultCellID, 2)
	if _, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:                   testDeploymentPublicID(t),
		OrgID:                      pgvalue.UUID(ids.orgID),
		CellID:                     dbtest.DefaultCellID,
		RouteGeneration:            2,
		ProjectID:                  pgvalue.UUID(ids.projectID),
		EnvironmentID:              pgvalue.UUID(ids.environmentID),
		Version:                    "queued.2",
		ApiVersion:                 "2026-06-06",
		SdkVersion:                 "",
		CliVersion:                 "",
		BundleFormatVersion:        2,
		WorkerProtocolVersion:      "helmr.worker.v1",
		WorkerGroupID:              pgvalue.UUID(workerGroupID),
		ContentHash:                contentHash,
		DeploymentSourceArtifactID: pgvalue.UUID(artifactID),
		Status:                     db.DeploymentStatusQueued,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTasksAllowSameTaskIDPerCell(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	secondCellID := "us-east-1-cell-2"
	ensureCellRoute(t, ctx, pool, ids, secondCellID, 2)
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (public_id, org_id, cell_id, project_id, environment_id, task_id)
		VALUES ($5, $1, $2, $3, $4, 'approval-task')
	`, ids.orgID, secondCellID, ids.projectID, ids.environmentID, testTaskPublicID(t)); err != nil {
		t.Fatal(err)
	}

	task, err := queries.GetTaskForStart(ctx, db.GetTaskForStartParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        secondCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		TaskID:        "approval-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.CellID != secondCellID {
		t.Fatalf("task cell = %q, want %q", task.CellID, secondCellID)
	}
}

func ensureCellRoute(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, cellID string, routeGeneration int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO cells (id, region_id, environment_class)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING
	`, cellID, dbtest.DefaultRegionID, dbtest.DefaultEnvironmentClass); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO org_cells (org_id, cell_id, role, state)
		VALUES ($1, $2, 'home', 'active')
		ON CONFLICT (org_id, cell_id, role) DO UPDATE
		   SET state = 'active'
	`, ids.orgID, cellID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cell_health (cell_id, state, routing_fresh_until)
		VALUES ($1, 'healthy', now() + interval '5 minutes')
		ON CONFLICT (cell_id) DO UPDATE
		   SET state = 'healthy',
		       routing_fresh_until = now() + interval '5 minutes'
	`, cellID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE environment_cells
		   SET route_state = 'draining'
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND region_id = $4
		   AND route_state = 'active'
	`, ids.orgID, ids.projectID, ids.environmentID, dbtest.DefaultRegionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO environment_cells (
			org_id, project_id, environment_id, region_id, cell_id, route_state, route_generation
		)
		VALUES ($1, $2, $3, $4, $5, 'active', $6)
	`, ids.orgID, ids.projectID, ids.environmentID, dbtest.DefaultRegionID, cellID, routeGeneration); err != nil {
		t.Fatal(err)
	}
}

func seedDeploymentInCell(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, cellID string, routeGeneration int64) uuid.UUID {
	t.Helper()
	workerGroupID := uuid.Must(uuid.NewV7())
	deploymentID := uuid.Must(uuid.NewV7())
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("deployment-" + cellID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_groups (id, cell_id, name)
		VALUES ($1, $2, 'default')
	`, workerGroupID, cellID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, cell_id, digest, size_bytes, media_type)
		VALUES ($1, $2, $3, 1, 'application/json')
	`, ids.orgID, cellID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, cell_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, $6, 'task_bundle', 1, 'application/json')
	`, artifactID, ids.orgID, cellID, ids.projectID, ids.environmentID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (
			id, public_id, org_id, cell_id, route_generation, project_id, environment_id, worker_group_id,
			version, content_hash, deployment_source_artifact_id, status
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, 'v2', $8, $9, 'deployed')
	`, deploymentID, ids.orgID, cellID, routeGeneration, ids.projectID, ids.environmentID, workerGroupID, digest, artifactID, testDeploymentPublicID(t)); err != nil {
		t.Fatal(err)
	}
	return deploymentID
}
