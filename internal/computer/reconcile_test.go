package computer

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
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

// sourceStore serves one reconcile target whose capture or restore source is
// read by a second statement that may observe a different checkpoint.
type sourceStore struct {
	reconcileTargetStore
	capture    db.ComputerCheckpoint
	restore    db.GetComputerInstanceRestoreCheckpointRow
	sourceErr  error
	memberRead bool
}

func (s *sourceStore) GetComputerInstanceCaptureCheckpoint(context.Context, db.GetComputerInstanceCaptureCheckpointParams) (db.ComputerCheckpoint, error) {
	return s.capture, s.sourceErr
}

func (s *sourceStore) GetComputerInstanceRestoreCheckpoint(context.Context, db.GetComputerInstanceRestoreCheckpointParams) (db.GetComputerInstanceRestoreCheckpointRow, error) {
	return s.restore, s.sourceErr
}

func (s *sourceStore) ListComputerCheckpointRuns(context.Context, db.ListComputerCheckpointRunsParams) ([]db.ComputerCheckpointRun, error) {
	s.memberRead = true
	return []db.ComputerCheckpointRun{}, nil
}

// checkpointingRow and restoringRow are discovered targets whose source
// checkpoint matches the checkpoint the store returns until a case changes it.
func checkpointingRow() (db.ListComputerInstanceReconcileTargetsRow, db.ComputerCheckpoint) {
	row := db.ListComputerInstanceReconcileTargetsRow{
		ID: pgvalue.UUID(uuid.NewV7()), ComputerID: pgvalue.UUID(uuid.NewV7()), ComputerSpecID: pgvalue.UUID(uuid.NewV7()), ProgramDeploymentID: pgvalue.UUID(uuid.NewV7()),
		CaptureCheckpointID: pgvalue.UUID(uuid.NewV7()), WriterGeneration: 3, MembershipRevision: 2,
		DesiredState: "ready", ObservedState: "ready", AdmissionState: "checkpointing",
	}
	cp := db.ComputerCheckpoint{ID: row.CaptureCheckpointID, ComputerID: row.ComputerID, SourceComputerInstanceID: row.ID, WriterGeneration: row.WriterGeneration, MembershipRevision: row.MembershipRevision, ComputerSpecID: row.ComputerSpecID, ProgramDeploymentID: row.ProgramDeploymentID}
	return row, cp
}

func restoringRow() (db.ListComputerInstanceReconcileTargetsRow, db.GetComputerInstanceRestoreCheckpointRow) {
	row := db.ListComputerInstanceReconcileTargetsRow{
		ID: pgvalue.UUID(uuid.NewV7()), ComputerID: pgvalue.UUID(uuid.NewV7()), ComputerSpecID: pgvalue.UUID(uuid.NewV7()), ProgramDeploymentID: pgvalue.UUID(uuid.NewV7()),
		SourceCheckpointID: pgvalue.UUID(uuid.NewV7()), PreparationDiskVersionID: pgvalue.UUID(uuid.NewV7()),
		DesiredState: "ready", ObservedState: "ready", AdmissionState: "restoring",
	}
	cp := db.ComputerCheckpoint{ID: row.SourceCheckpointID, ComputerID: row.ComputerID, ComputerSpecID: row.ComputerSpecID, ProgramDeploymentID: row.ProgramDeploymentID, PrivateComputerDiskVersionID: row.PreparationDiskVersionID}
	return row, db.GetComputerInstanceRestoreCheckpointRow{ComputerCheckpoint: cp}
}

// A capture source that changed or disappeared between target discovery and
// source loading fails the read instead of returning a capture payload.
func TestReconcileTargetsRejectsChangedCaptureSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*db.ComputerCheckpoint) error
	}{
		{"unchanged", func(*db.ComputerCheckpoint) error { return nil }},
		{"checkpoint", func(cp *db.ComputerCheckpoint) error { cp.ID = pgvalue.UUID(uuid.NewV7()); return nil }},
		{"source instance", func(cp *db.ComputerCheckpoint) error {
			cp.SourceComputerInstanceID = pgvalue.UUID(uuid.NewV7())
			return nil
		}},
		{"writer generation", func(cp *db.ComputerCheckpoint) error { cp.WriterGeneration++; return nil }},
		{"membership", func(cp *db.ComputerCheckpoint) error { cp.MembershipRevision++; return nil }},
		{"spec", func(cp *db.ComputerCheckpoint) error { cp.ComputerSpecID = pgvalue.UUID(uuid.NewV7()); return nil }},
		{"deployment", func(cp *db.ComputerCheckpoint) error { cp.ProgramDeploymentID = pgvalue.UUID(uuid.NewV7()); return nil }},
		{"missing", func(*db.ComputerCheckpoint) error { return pgx.ErrNoRows }},
	} {
		t.Run(test.name, func(t *testing.T) {
			row, cp := checkpointingRow()
			sourceErr := test.change(&cp)
			store := &sourceStore{reconcileTargetStore: reconcileTargetStore{rows: []db.ListComputerInstanceReconcileTargetsRow{row}}, capture: cp, sourceErr: sourceErr}
			targets, err := ReconcileTargets(t.Context(), store, Host{GroupID: uuid.NewV7(), HostID: uuid.NewV7(), Epoch: 1}, 64)
			if test.name == "unchanged" {
				if err != nil || len(targets) != 1 || targets[0].Action != ReconcileCapture || targets[0].Capture == nil || targets[0].Capture.Checkpoint.ID != row.CaptureCheckpointID {
					t.Fatalf("capture target = %+v %v", targets, err)
				}
				return
			}
			if err == nil || targets != nil || store.memberRead {
				t.Fatalf("changed capture source read: targets=%+v members=%v err=%v", targets, store.memberRead, err)
			}
			if test.name == "missing" && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("missing capture source = %v", err)
			}
		})
	}
}

// A restore source that changed or disappeared between target discovery and
// source loading fails the read instead of returning a restore payload.
func TestReconcileTargetsRejectsChangedRestoreSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*db.ComputerCheckpoint) error
	}{
		{"unchanged", func(*db.ComputerCheckpoint) error { return nil }},
		{"checkpoint", func(cp *db.ComputerCheckpoint) error { cp.ID = pgvalue.UUID(uuid.NewV7()); return nil }},
		{"computer", func(cp *db.ComputerCheckpoint) error { cp.ComputerID = pgvalue.UUID(uuid.NewV7()); return nil }},
		{"spec", func(cp *db.ComputerCheckpoint) error { cp.ComputerSpecID = pgvalue.UUID(uuid.NewV7()); return nil }},
		{"deployment", func(cp *db.ComputerCheckpoint) error { cp.ProgramDeploymentID = pgvalue.UUID(uuid.NewV7()); return nil }},
		{"private disk version", func(cp *db.ComputerCheckpoint) error {
			cp.PrivateComputerDiskVersionID = pgvalue.UUID(uuid.NewV7())
			return nil
		}},
		{"missing", func(*db.ComputerCheckpoint) error { return pgx.ErrNoRows }},
	} {
		t.Run(test.name, func(t *testing.T) {
			row, authority := restoringRow()
			sourceErr := test.change(&authority.ComputerCheckpoint)
			store := &sourceStore{reconcileTargetStore: reconcileTargetStore{rows: []db.ListComputerInstanceReconcileTargetsRow{row}}, restore: authority, sourceErr: sourceErr}
			targets, err := ReconcileTargets(t.Context(), store, Host{GroupID: uuid.NewV7(), HostID: uuid.NewV7(), Epoch: 1}, 64)
			if test.name == "unchanged" {
				if err != nil || len(targets) != 1 || targets[0].Action != ReconcilePrepare || targets[0].Restore == nil || targets[0].Restore.Checkpoint.ComputerCheckpoint.ID != row.SourceCheckpointID {
					t.Fatalf("restore target = %+v %v", targets, err)
				}
				return
			}
			if err == nil || targets != nil || store.memberRead {
				t.Fatalf("changed restore source read: targets=%+v members=%v err=%v", targets, store.memberRead, err)
			}
			if test.name == "missing" && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("missing restore source = %v", err)
			}
		})
	}
}
