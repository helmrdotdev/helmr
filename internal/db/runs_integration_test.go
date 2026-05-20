package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestListRunSummariesRunningFilterIncludesLeasedRuns(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	leasedRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	runningRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	succeededRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	for _, run := range []struct {
		id     pgtype.UUID
		status db.RunStatus
	}{
		{id: leasedRunID, status: db.RunStatusLeased},
		{id: runningRunID, status: db.RunStatusRunning},
		{id: succeededRunID, status: db.RunStatusSucceeded},
	} {
		if _, err := pool.Exec(ctx, `
UPDATE runs
   SET status = $3::run_status,
       updated_at = now()
 WHERE org_id = $1
   AND id = $2
`, orgID, run.id, run.status); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := queries.ListRunSummaries(ctx, db.ListRunSummariesParams{
		OrgID:        orgID,
		StatusFilter: "running",
		RowLimit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[db.RunStatus]int{}
	for _, row := range rows {
		got[row.Status]++
	}
	if len(rows) != 2 || got[db.RunStatusLeased] != 1 || got[db.RunStatusRunning] != 1 {
		t.Fatalf("running summary statuses = %+v, rows = %+v", got, rows)
	}

	scopedRows, err := queries.ListScopedRunSummaries(ctx, db.ListScopedRunSummariesParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		StatusFilter:  "running",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	got = map[db.RunStatus]int{}
	for _, row := range scopedRows {
		got[row.Status]++
	}
	if len(scopedRows) != 2 || got[db.RunStatusLeased] != 1 || got[db.RunStatusRunning] != 1 {
		t.Fatalf("scoped running summary statuses = %+v, rows = %+v", got, scopedRows)
	}
}
