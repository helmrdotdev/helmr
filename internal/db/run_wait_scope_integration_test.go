package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestGetWorkerRunWaitScopeUsesWorkerGroupIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)

	scope, err := queries.GetWorkerRunWaitScope(ctx, db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pgvalue.MustUUIDValue(scope.RunID), ids.runID; got != want {
		t.Fatalf("run id = %s, want %s", got, want)
	}
	if got, want := pgvalue.MustUUIDValue(scope.CurrentRunLeaseID), runLeaseID; got != want {
		t.Fatalf("run lease id = %s, want %s", got, want)
	}
	if got := scope.WorkerCniProfile; got != "default" {
		t.Fatalf("worker cni profile = %q, want default", got)
	}
	if !scope.MaterializationID.Valid {
		t.Fatal("materialization id is invalid")
	}
}
