package controlplane

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

type runStartArm struct {
	mode                 runLeaseClaimMode
	runWaitID            pgtype.UUID
	checkpointID         pgtype.UUID
	resumeAttachID       pgtype.UUID
	resumeRequestVersion int64
}

type runStartValidationAuthority struct {
	run            db.Run
	parentRun      db.Run
	runLease       db.RunLease
	runtime        db.RuntimeInstance
	workspace      db.Workspace
	workspaceMount db.WorkspaceMount
	runWait        db.RunWait
}

func parseRunStartArm(request workerapi.RunStartRequest) (runStartArm, error) {
	parseIDs := func(runWait, checkpoint, attach string) (pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
		values := []string{runWait, checkpoint, attach}
		parsed := make([]pgtype.UUID, len(values))
		for index, raw := range values {
			value, err := ids.Parse(raw)
			if err != nil {
				return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("run start arm IDs must be canonical UUIDv7 values")
			}
			parsed[index] = pgvalue.UUID(value)
		}
		return parsed[0], parsed[1], parsed[2], nil
	}
	switch {
	case request.Fresh != nil:
		return runStartArm{mode: runLeaseClaimFresh}, nil
	case request.Restore != nil:
		waitID, checkpointID, attachID, err := parseIDs(
			request.Restore.RunWaitID, request.Restore.CheckpointID, request.Restore.ResumeAttachID,
		)
		if err != nil || request.Restore.ResumeRequestVersion <= 0 {
			return runStartArm{}, errors.New("restore IDs must be UUIDs and resume_request_version must be positive")
		}
		return runStartArm{mode: runLeaseClaimRestore, runWaitID: waitID, checkpointID: checkpointID,
			resumeAttachID: attachID, resumeRequestVersion: request.Restore.ResumeRequestVersion}, nil
	default:
		return runStartArm{}, errors.New("exactly one fresh or restore run start arm is required")
	}
}

func deriveRunStartMode(locators db.GetRunLeaseStartLocatorsRow) runLeaseClaimMode {
	if locators.RunWaitID.Valid {
		return runLeaseClaimRestore
	}
	return runLeaseClaimFresh
}

func sameWorkspaceParentResumeWait(wait db.RunWait) bool {
	return wait.Kind == db.WaitKindChild &&
		wait.ChildParentOwned.Valid && wait.ChildParentOwned.Bool &&
		wait.OwnershipGeneration.Valid
}
