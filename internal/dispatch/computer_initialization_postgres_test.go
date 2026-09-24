package dispatch

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"testing"
)

func TestActorInitialPreparationRejectsCancelledRun(t *testing.T) {
	f := newRunPlacementFixture(t)
	convertFreshRunToActor(t, f)
	runtimeID, _ := prepareInitialGeneration(t, f)
	fence := ComputerPreparationFence{RuntimeID: runtimeID, WorkerID: pgvalue.UUID(f.workerID), WorkerGroupID: f.groupID, WorkerEpoch: 1, DesiredVersion: 1}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := LockComputerPreparation(f.ctx, tx, fence)
	if err != nil {
		_ = tx.Rollback(f.ctx)
		t.Fatal(err)
	}
	if err = authority.CheckDeadlines(f.ctx, tx); err != nil {
		_ = tx.Rollback(f.ctx)
		t.Fatal(err)
	}
	if err = tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	canceler, err := run.NewCanceler(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	cancellation, err := canceler.Cancel(f.ctx, run.CancellationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: f.runID})
	if err != nil || cancellation.Actor == nil || cancellation.Actor.Status != "accepted" {
		t.Fatalf("cancel preparing Actor: %+v %v", cancellation, err)
	}

	tx, err = f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = LockComputerPreparation(f.ctx, tx, fence); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelled Actor can publish initialization: %v", err)
	}
}
