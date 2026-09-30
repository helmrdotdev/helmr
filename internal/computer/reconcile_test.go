package computer

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestReconcileActionPrecedence(t *testing.T) {
	for _, test := range []struct {
		observed, desired, admission string
		want                         ReconcileAction
	}{
		{"ready", "ready", "checkpointing", ReconcileCapture},
		{"ready", "closed", "checkpointing", ReconcileClose},
		{"failed", "closed", "checkpointing", ReconcileReclaim},
		{"lost", "ready", "checkpointing", ReconcileReclaim},
		{"allocated", "ready", "restoring", ReconcilePrepare},
		{"ready", "ready", "open", ReconcilePrepare},
	} {
		row := db.ListComputerInstanceReconcileTargetsRow{ObservedState: test.observed, DesiredState: test.desired, AdmissionState: test.admission}
		if got := reconcileAction(row); got != test.want {
			t.Fatalf("%+v: %d", test, got)
		}
	}
}

type reconcileTargetStore struct {
	db.Querier
	rows   []db.ListComputerInstanceReconcileTargetsRow
	params db.ListComputerInstanceReconcileTargetsParams
}

func (s *reconcileTargetStore) ListComputerInstanceReconcileTargets(_ context.Context, params db.ListComputerInstanceReconcileTargetsParams) ([]db.ListComputerInstanceReconcileTargetsRow, error) {
	s.params = params
	return s.rows, nil
}

// Close and reclaim targets need no capture or restore source.
func TestReconcileTargetsReadsBoundedBatchOfHostEpoch(t *testing.T) {
	host := Host{GroupID: uuid.NewV7(), HostID: uuid.NewV7(), Epoch: 7}
	store := &reconcileTargetStore{rows: []db.ListComputerInstanceReconcileTargetsRow{
		{ID: pgvalue.UUID(uuid.NewV7()), WorkerEpoch: 7, DesiredState: "closed", ObservedState: "ready"},
		{ID: pgvalue.UUID(uuid.NewV7()), WorkerEpoch: 7, DesiredState: "closed", ObservedState: "failed"},
	}}
	targets, err := ReconcileTargets(t.Context(), store, host, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].Action != ReconcileClose || targets[1].Action != ReconcileReclaim || targets[0].Capture != nil || targets[0].Restore != nil {
		t.Fatalf("targets = %+v", targets)
	}
	if store.params.RowLimit != 64 || store.params.WorkerEpoch != 7 || store.params.WorkerGroupID != pgvalue.UUID(host.GroupID) || store.params.WorkerHostID != pgvalue.UUID(host.HostID) {
		t.Fatalf("params = %+v", store.params)
	}
}

// A restoring target whose checkpoint provenance is only historical, not a
// restore in progress, is prepared without a restore source.
func TestReconcileTargetsDoesNotRestoreOpenedCheckpoint(t *testing.T) {
	store := &reconcileTargetStore{rows: []db.ListComputerInstanceReconcileTargetsRow{
		{ID: pgvalue.UUID(uuid.NewV7()), DesiredState: "ready", ObservedState: "ready", AdmissionState: "open", SourceCheckpointID: pgvalue.UUID(uuid.NewV7())},
	}}
	targets, err := ReconcileTargets(t.Context(), store, Host{GroupID: uuid.NewV7(), HostID: uuid.NewV7(), Epoch: 7}, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Action != ReconcilePrepare || targets[0].Restore != nil {
		t.Fatalf("checkpoint provenance triggered restore: %+v", targets)
	}
}
