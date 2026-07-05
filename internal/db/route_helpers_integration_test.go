package db_test

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/cell"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgxpool"
)

func routeEnvironmentToOtherCell(t testingT, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) string {
	t.Helper()
	otherCellID := dbtest.DefaultCellID + "-other"
	queries := db.New(pool)
	if _, err := queries.EnsureCell(ctx, db.EnsureCellParams{
		ID:               otherCellID,
		RegionID:         dbtest.DefaultRegionID,
		EnvironmentClass: dbtest.DefaultEnvironmentClass,
		State:            db.CellStateActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cell.ReportHealth(ctx, queries, cell.HealthConfig{
		CellID:             otherCellID,
		Component:          cell.ComponentDispatcher,
		RequiredComponents: cell.RoutingRequiredComponents(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnsureOrgCell(ctx, db.EnsureOrgCellParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		CellID: otherCellID,
		Role:   db.OrgCellRoleHome,
		State:  db.OrgCellStateActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cell.EnsureEnvironmentRoute(ctx, queries, cell.EnsureEnvironmentRouteParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RegionID:      dbtest.DefaultRegionID,
		LocalCellID:   otherCellID,
	}); err != nil {
		t.Fatal(err)
	}
	return otherCellID
}

func disableDefaultEnvironmentRoute(t testingT, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) {
	t.Helper()
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
}

type testingT interface {
	Helper()
	Fatal(args ...any)
}
