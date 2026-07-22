package control

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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

func parseRunStartArm(request api.WorkerRunStartRequest) (runStartArm, error) {
	parseIDs := func(runWait, checkpoint, attach string) (pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
		values := []string{runWait, checkpoint, attach}
		parsed := make([]pgtype.UUID, len(values))
		for index, raw := range values {
			value, err := uuid.Parse(strings.TrimSpace(raw))
			if err != nil {
				return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("Run start arm IDs must be UUIDs")
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
	case request.Attach != nil && request.Attach.Child != nil:
		waitID, checkpointID, attachID, err := parseIDs(
			request.Attach.Child.RunWaitID, request.Attach.Child.CheckpointID, request.Attach.Child.ResumeAttachID,
		)
		if err != nil {
			return runStartArm{}, errors.New("attach.child IDs must be UUIDs")
		}
		return runStartArm{mode: runLeaseClaimAttachChild, runWaitID: waitID,
			checkpointID: checkpointID, resumeAttachID: attachID}, nil
	case request.Attach != nil && request.Attach.Parent != nil:
		waitID, checkpointID, attachID, err := parseIDs(
			request.Attach.Parent.RunWaitID, request.Attach.Parent.CheckpointID, request.Attach.Parent.ResumeAttachID,
		)
		if err != nil || request.Attach.Parent.ResumeRequestVersion <= 0 {
			return runStartArm{}, errors.New("attach.parent IDs must be UUIDs and resume_request_version must be positive")
		}
		return runStartArm{mode: runLeaseClaimAttachParent, runWaitID: waitID, checkpointID: checkpointID,
			resumeAttachID: attachID, resumeRequestVersion: request.Attach.Parent.ResumeRequestVersion}, nil
	default:
		return runStartArm{}, errors.New("Run start arm is required")
	}
}

func deriveRunStartMode(locators db.GetRunLeaseStartLocatorsRow) (runLeaseClaimMode, error) {
	if locators.RunWaitID.Valid {
		if locators.ResumeChildRunID.Valid &&
			locators.ResumeChildParentOwned.Valid &&
			locators.ResumeChildParentOwned.Bool {
			return runLeaseClaimAttachParent, nil
		}
		return runLeaseClaimRestore, nil
	}
	if locators.EnclosingWaitID.Valid {
		return runLeaseClaimAttachChild, nil
	}
	return runLeaseClaimFresh, nil
}
