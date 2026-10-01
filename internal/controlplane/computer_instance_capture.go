package controlplane

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func projectComputerInstanceCapture(cp db.ComputerCheckpoint, members []db.ComputerCheckpointRun) (*workerapi.RuntimeCapture, error) {
	if (cp.Status != "creating" && cp.Status != "aborted") || !cp.ID.Valid || !cp.ComputerID.Valid || !cp.EnvironmentID.Valid || !cp.SourceComputerInstanceID.Valid || !cp.ComputerSpecID.Valid || cp.WriterGeneration <= 0 || cp.MembershipRevision < 0 || (len(members) > 0 && !cp.ProgramDeploymentID.Valid) {
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
