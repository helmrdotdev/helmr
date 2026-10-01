package computer

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// ReconcileAction is what a worker host must do to converge an Instance.
type ReconcileAction uint8

const (
	// ReconcilePrepare prepares the Instance, restoring its checkpoint when
	// the Instance is restoring.
	ReconcilePrepare ReconcileAction = iota
	// ReconcileCapture captures the Instance's checkpoint.
	ReconcileCapture
	// ReconcileClose closes the Instance.
	ReconcileClose
	// ReconcileReclaim reclaims a failed or lost Instance.
	ReconcileReclaim
)

// ReconcileTarget is an Instance a worker host must converge, with the
// capture or restore source its action needs.
type ReconcileTarget struct {
	Instance db.ListComputerInstanceReconcileTargetsRow
	Action   ReconcileAction
	// Capture is the checkpoint a ReconcileCapture target captures.
	Capture *CaptureSource
	// Restore is the checkpoint a restoring ReconcilePrepare target restores.
	Restore *RestoreSource
}

// CaptureSource is a creating checkpoint and its captured members.
type CaptureSource struct {
	Checkpoint db.ComputerCheckpoint
	Members    []db.ComputerCheckpointRun
}

// RestoreSource is a ready checkpoint with its retained artifacts and its
// captured members.
type RestoreSource struct {
	Checkpoint db.GetComputerInstanceRestoreCheckpointRow
	Members    []db.ComputerCheckpointRun
}

// ReconcileTargets reads up to limit Instances the worker epoch must converge,
// without locks. A capture or restore source that changed since the
// Instance was read fails the read.
func ReconcileTargets(ctx context.Context, q db.Querier, host Host, limit int32) ([]ReconcileTarget, error) {
	rows, err := q.ListComputerInstanceReconcileTargets(ctx, db.ListComputerInstanceReconcileTargetsParams{
		WorkerGroupID: pgvalue.UUID(host.GroupID), WorkerHostID: pgvalue.UUID(host.HostID), WorkerEpoch: host.Epoch,
		RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list computer instance reconcile targets: %w", err)
	}
	targets := make([]ReconcileTarget, 0, len(rows))
	for _, row := range rows {
		target := ReconcileTarget{Instance: row, Action: reconcileAction(row)}
		switch {
		case target.Action == ReconcileCapture:
			if target.Capture, err = loadCaptureSource(ctx, q, row); err != nil {
				return nil, err
			}
		case target.Action == ReconcilePrepare && row.AdmissionState == "restoring":
			if target.Restore, err = loadRestoreSource(ctx, q, row); err != nil {
				return nil, err
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func reconcileAction(row db.ListComputerInstanceReconcileTargetsRow) ReconcileAction {
	switch {
	case row.ObservedState == "failed" || row.ObservedState == "lost":
		return ReconcileReclaim
	case row.DesiredState == "closed":
		return ReconcileClose
	case row.AdmissionState == "checkpointing":
		return ReconcileCapture
	default:
		return ReconcilePrepare
	}
}

func loadCaptureSource(ctx context.Context, q db.Querier, row db.ListComputerInstanceReconcileTargetsRow) (*CaptureSource, error) {
	if row.AdmissionState != "checkpointing" || !row.CaptureCheckpointID.Valid {
		return nil, errors.New("computer capture source is incomplete")
	}
	cp, err := q.GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{
		ComputerInstanceID: row.ID, EnvironmentID: row.EnvironmentID, WorkerGroupID: row.WorkerGroupID,
		WorkerHostID: row.WorkerHostID, WorkerEpoch: row.WorkerEpoch, DesiredVersion: row.DesiredVersion,
		WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("load computer capture checkpoint: %w", err)
	}
	if cp.ID != row.CaptureCheckpointID || cp.ComputerID != row.ComputerID || cp.SourceComputerInstanceID != row.ID || cp.WriterGeneration != row.WriterGeneration || cp.MembershipRevision != row.MembershipRevision || cp.ComputerSpecID != row.ComputerSpecID || cp.ProgramDeploymentID != row.ProgramDeploymentID {
		return nil, errors.New("computer capture source changed during discovery")
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: row.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return nil, fmt.Errorf("load computer capture membership: %w", err)
	}
	return &CaptureSource{Checkpoint: cp, Members: members}, nil
}

func loadRestoreSource(ctx context.Context, q db.Querier, row db.ListComputerInstanceReconcileTargetsRow) (*RestoreSource, error) {
	if !row.SourceCheckpointID.Valid {
		return nil, errors.New("computer restore destination is incomplete")
	}
	authority, err := q.GetComputerInstanceRestoreCheckpoint(ctx, db.GetComputerInstanceRestoreCheckpointParams{
		ComputerInstanceID: row.ID, EnvironmentID: row.EnvironmentID, WorkerGroupID: row.WorkerGroupID,
		WorkerHostID: row.WorkerHostID, WorkerEpoch: row.WorkerEpoch, DesiredVersion: row.DesiredVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("load computer restore checkpoint: %w", err)
	}
	cp := authority.ComputerCheckpoint
	if cp.ID != row.SourceCheckpointID || cp.ComputerID != row.ComputerID || cp.ComputerSpecID != row.ComputerSpecID ||
		cp.ProgramDeploymentID != row.ProgramDeploymentID || cp.PrivateComputerDiskVersionID != row.PreparationDiskVersionID {
		return nil, errors.New("computer restore source changed during discovery")
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: row.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return nil, fmt.Errorf("load computer restore membership: %w", err)
	}
	return &RestoreSource{Checkpoint: authority, Members: members}, nil
}
