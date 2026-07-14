package dispatch

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type checkpointDiscoveryFake struct {
	row db.ListDueRunCheckpointWaitsRow
}

func (f checkpointDiscoveryFake) ListDueRunCheckpointWaits(context.Context, int32) ([]db.ListDueRunCheckpointWaitsRow, error) {
	return []db.ListDueRunCheckpointWaitsRow{f.row}, nil
}

type checkpointAuthorityFake struct {
	params db.ClaimRunCheckpointWaitParams
	row    db.ClaimRunCheckpointWaitRow
}

func (f *checkpointAuthorityFake) RequestCheckpoint(_ context.Context, params db.ClaimRunCheckpointWaitParams) (db.ClaimRunCheckpointWaitRow, error) {
	f.params = params
	return f.row, nil
}

func TestCheckpointReconcilerPublishesCommittedWorkerEpochRuntimeWake(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	waitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	leaseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workerID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runtimeID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	discovery := checkpointDiscoveryFake{row: db.ListDueRunCheckpointWaitsRow{
		OrgID: orgID, RunID: runID, RunWaitID: waitID, RunLeaseID: leaseID, ExpectedRunStateVersion: 4,
	}}
	authority := &checkpointAuthorityFake{row: db.ClaimRunCheckpointWaitRow{
		ID: waitID, WorkerInstanceID: workerID, WorkerEpoch: 7, RuntimeInstanceID: runtimeID,
		CheckpointRequestVersion: 2,
	}}
	publisher := &fakeWakePublisher{}
	reconciler, err := NewCheckpointReconciler(discovery, authority, publisher, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if authority.params.RunLeaseID != leaseID || authority.params.ExpectedRunStateVersion != 4 {
		t.Fatalf("checkpoint authority params = %+v", authority.params)
	}
	if len(publisher.wakes) != 1 {
		t.Fatalf("published wakes = %d, want 1", len(publisher.wakes))
	}
	wake := publisher.wakes[0]
	if wake.Domain != "checkpoint" || wake.WorkerID != workerID || wake.WorkerEpoch != 7 ||
		wake.RuntimeID != runtimeID || wake.AuthorityID != waitID || wake.RequestVersion != 2 {
		t.Fatalf("checkpoint wake = %+v", wake)
	}
}
