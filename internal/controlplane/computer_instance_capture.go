package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func computerInstanceReconcileAction(row db.ListComputerInstanceReconcileTargetsRow) string {
	switch {
	case row.ObservedState == "failed" || row.ObservedState == "lost":
		return workerapi.RuntimeReconcileReclaim
	case row.DesiredState == "closed":
		return workerapi.RuntimeReconcileClose
	case row.AdmissionState == "checkpointing":
		return workerapi.RuntimeReconcileCapture
	default:
		return workerapi.RuntimeReconcilePrepare
	}
}

func loadComputerInstanceCapture(ctx context.Context, store db.Querier, row db.ListComputerInstanceReconcileTargetsRow) (*workerapi.RuntimeCapture, error) {
	if store == nil || row.AdmissionState != "checkpointing" || !row.CaptureCheckpointID.Valid {
		return nil, errors.New("computer capture source is incomplete")
	}
	cp, err := store.GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{
		ComputerInstanceID: row.ID, EnvironmentID: row.EnvironmentID, WorkerGroupID: row.WorkerGroupID,
		WorkerHostID: row.WorkerHostID, WorkerEpoch: row.WorkerEpoch, DesiredVersion: row.DesiredVersion,
		WorkerFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("load computer capture checkpoint: %w", err)
	}
	if cp.ID != row.CaptureCheckpointID || cp.ComputerID != row.ComputerID || cp.SourceComputerInstanceID != row.ID || cp.WriterGeneration != row.WriterGeneration || cp.MembershipRevision != row.MembershipRevision || cp.ComputerSpecID != row.ComputerSpecID || cp.ProgramDeploymentID != row.ProgramDeploymentID {
		return nil, errors.New("computer capture source changed during discovery")
	}
	members, err := store.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: row.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return nil, fmt.Errorf("load computer capture membership: %w", err)
	}
	return projectComputerInstanceCapture(cp, members)
}

func projectComputerInstanceCapture(cp db.ComputerCheckpoint, members []db.ComputerCheckpointRun) (*workerapi.RuntimeCapture, error) {
	if cp.Status != "creating" || !cp.ID.Valid || !cp.ComputerID.Valid || !cp.EnvironmentID.Valid || !cp.SourceComputerInstanceID.Valid || !cp.ComputerSpecID.Valid || cp.WriterGeneration <= 0 || cp.MembershipRevision < 0 || (len(members) > 0 && !cp.ProgramDeploymentID.Valid) {
		return nil, errors.New("computer capture identity is incomplete")
	}
	result := &workerapi.RuntimeCapture{CheckpointID: pgvalue.UUIDString(cp.ID), MembershipRevision: cp.MembershipRevision, ProgramDeploymentID: pgvalue.UUIDString(cp.ProgramDeploymentID), Runs: make([]workerapi.RuntimeCaptureRun, 0, len(members))}
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		id := pgvalue.UUIDString(member.RunID)
		if _, duplicate := seen[id]; duplicate || !member.RunID.Valid || !member.RunWaitID.Valid || !member.SourceRunLeaseID.Valid || member.AttemptNumber <= 0 || member.CheckpointID != cp.ID || member.EnvironmentID != cp.EnvironmentID || member.ComputerID != cp.ComputerID || member.SourceComputerInstanceID != cp.SourceComputerInstanceID || member.WriterGeneration != cp.WriterGeneration || (member.ActorSpeculativeInputSequence.Valid && member.ActorSpeculativeInputSequence.Int64 < 0) {
			return nil, errors.New("computer capture membership has inconsistent authority")
		}
		seen[id] = struct{}{}
		run := workerapi.RuntimeCaptureRun{RunID: id, AttemptNumber: member.AttemptNumber, RunWaitID: pgvalue.UUIDString(member.RunWaitID), RunLeaseID: pgvalue.UUIDString(member.SourceRunLeaseID)}
		if member.ActorSpeculativeInputSequence.Valid {
			cursor := member.ActorSpeculativeInputSequence.Int64
			run.ActorSpeculativeInputSequence = &cursor
		}
		result.Runs = append(result.Runs, run)
	}
	return result, nil
}
