package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestGetWorkerRunWaitScopeUsesWorkerGroupIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
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
	if !scope.WorkspaceMountID.Valid {
		t.Fatal("workspaceMount id is invalid")
	}
}

func TestGetWorkerRunWaitScopeRejectsDisabledSourceRoute(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	disableDefaultEnvironmentRoute(t, ctx, pool, ids)

	_, err := queries.GetWorkerRunWaitScope(ctx, db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetWorkerRunWaitScope disabled route error = %v, want pgx.ErrNoRows", err)
	}
}
