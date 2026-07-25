package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
)

type resumedProgramAdmission struct {
	runWaitID            string
	checkpointID         string
	resumeAttachID       string
	resumeRequestVersion int64
	correlationID        string
	entrypointKind       string
	entrypointDeclaredID string
	decision             api.WorkerRunLeaseDecision
	start                api.WorkerRunStartRequest
	recreated            bool
}

func (r ProgramRunner) startResumedProgram(
	ctx context.Context,
	claim *api.WorkerRunLeaseClaimResponse,
	control RunLeaseControl,
) (freshProgram, error) {
	resume, err := validateResumedProgramClaim(claim)
	if err != nil {
		return freshProgram{}, err
	}
	defer func() {
		claim.Workspace.WriteCapability = ""
	}()
	waitClient, ok := control.(RunWaitClient)
	if !ok {
		return freshProgram{}, errors.New("restored Program wait control is required")
	}
	admissionCtx, cancelAdmission := context.WithDeadline(ctx, claim.Lease.ExpiresAt)
	defer cancelAdmission()
	opened, err := r.WorkspaceMounts.OpenWorkspaceMountSession(admissionCtx, claim.Lease.WorkspaceMountID)
	if err != nil {
		return freshProgram{}, err
	}
	keepSession := false
	defer func() {
		if !keepSession {
			_ = opened.Session.Close(context.Background())
		}
	}()
	if opened.ControlSession == nil || opened.Session.Stream() == nil {
		return freshProgram{}, errors.New("restored Workspace mount control and resume streams are required")
	}
	if err := validateResumedProgramMount(
		claim.Lease,
		opened.Mount,
		resume,
	); err != nil {
		return freshProgram{}, err
	}
	authority := freshWorkspaceAuthority(claim, opened.ChannelToken)
	grant := &workspacev0.GrantProgramResumeRequest{
		Authority: authority, RunWaitId: resume.runWaitID, CheckpointId: resume.checkpointID,
		ResumeAttachId: resume.resumeAttachID, ResumeRequestVersion: resume.resumeRequestVersion,
		CorrelationId: resume.correlationID,
	}
	if err := grantProgramResumeOnSession(admissionCtx, opened.ControlSession, grant); err != nil {
		return freshProgram{}, fmt.Errorf("install resumed Program authority: %w", err)
	}
	var startResponse api.WorkerRunStartResponse
	if err := retryRunLeaseRequest(admissionCtx, func(requestCtx context.Context) error {
		var requestErr error
		resume.start.Lease = claim.Lease
		startResponse, requestErr = control.AcknowledgeRunStart(
			requestCtx,
			resume.start,
		)
		return requestErr
	}); err != nil {
		return freshProgram{}, fmt.Errorf("acknowledge resumed Run start: %w", err)
	}
	if !equalRunLeaseReceipt(startResponse.Lease, claim.Lease) {
		return freshProgram{}, errors.New("resumed Run start acknowledgement changed the Run Lease receipt")
	}
	attach := &runv0.ResumeAttach{
		RunId: claim.Lease.RunID, AttemptNumber: uint32(claim.Lease.AttemptNumber),
		RunLeaseId: claim.Lease.ID, RunWaitId: resume.runWaitID, CheckpointId: resume.checkpointID,
		ResumeAttachId: resume.resumeAttachID, ResumeRequestVersion: resume.resumeRequestVersion,
		CorrelationId: resume.correlationID,
	}
	if err := frameio.WriteProtoFrame(opened.Session.Stream(), attach); err != nil {
		return freshProgram{}, fmt.Errorf("attach resumed Program: %w", err)
	}
	kind, data, noResult, err := restoredProgramDecision(resume.decision)
	if err != nil {
		return freshProgram{}, err
	}
	decision := &runv0.ResumeDecision{
		RunWaitId: resume.runWaitID, Kind: kind, DataJson: string(data), RequireConsumedAck: true,
		CheckpointId: resume.checkpointID, ResumeAttachId: resume.resumeAttachID,
		ResumeRequestVersion: resume.resumeRequestVersion, RunLeaseId: claim.Lease.ID,
		CorrelationId: resume.correlationID, NoResult: noResult,
	}
	if err := frameio.WriteProtoFrame(opened.Session.Stream(), decision); err != nil {
		return freshProgram{}, fmt.Errorf("apply resumed Program decision: %w", err)
	}
	ackCtx, cancelAck := context.WithTimeout(admissionCtx, restoreAttachTimeout)
	ack, err := readResumeAck(ackCtx, opened.Session)
	cancelAck()
	if err != nil {
		return freshProgram{}, fmt.Errorf("read resumed Program proof: %w", err)
	}
	if ack.GetRunWaitId() != resume.runWaitID || ack.GetCheckpointId() != resume.checkpointID ||
		ack.GetResumeAttachId() != resume.resumeAttachID ||
		ack.GetResumeRequestVersion() != resume.resumeRequestVersion ||
		ack.GetRunLeaseId() != claim.Lease.ID || ack.GetCorrelationId() != resume.correlationID {
		return freshProgram{}, errors.New("resumed Program proof did not match exact authority")
	}
	release := RestoreAcknowledgement{
		Lease: claim.Lease, RunWaitID: resume.runWaitID, CheckpointID: resume.checkpointID,
		ResumeAttachID: resume.resumeAttachID, ResumeRequestVersion: resume.resumeRequestVersion,
		CorrelationID: resume.correlationID,
	}
	if err := retryRunLeaseRequest(admissionCtx, func(requestCtx context.Context) error {
		return (ControlRunWaits{Client: waitClient}).AcknowledgeRestore(requestCtx, release)
	}); err != nil {
		return freshProgram{}, fmt.Errorf("release resumed Run Wait: %w", err)
	}
	entrypoint, err := resumedEntrypoint(
		resume.entrypointKind,
		resume.entrypointDeclaredID,
	)
	if err != nil {
		return freshProgram{}, err
	}
	keepSession = true
	return freshProgram{
		session: opened.Session, mount: opened.Mount, lease: claim.Lease, authority: authority,
		entrypoint: entrypoint,
	}, nil
}

func validateResumedProgramClaim(
	claim *api.WorkerRunLeaseClaimResponse,
) (resumedProgramAdmission, error) {
	if claim == nil {
		return resumedProgramAdmission{}, errors.New("Run Lease claim is required")
	}
	lease := claim.Lease
	if strings.TrimSpace(lease.ID) == "" || strings.TrimSpace(lease.RunID) == "" || lease.AttemptNumber <= 0 ||
		lease.LeaseSequence <= 0 || strings.TrimSpace(lease.RuntimeInstanceID) == "" ||
		strings.TrimSpace(lease.RuntimeIdentityID) == "" || strings.TrimSpace(lease.WorkspaceID) == "" ||
		strings.TrimSpace(lease.WorkspaceMountID) == "" || strings.TrimSpace(lease.WorkspaceLeaseID) == "" ||
		strings.TrimSpace(lease.BaseWorkspaceVersionID) == "" || lease.MountFencingGeneration <= 0 ||
		lease.StartDeadlineAt.IsZero() ||
		!lease.StartDeadlineAt.After(time.Now()) ||
		lease.ExpiresAt.IsZero() || !lease.ExpiresAt.After(time.Now()) ||
		strings.TrimSpace(claim.Workspace.WriteCapability) == "" {
		return resumedProgramAdmission{}, errors.New("resumed Program Run Lease receipt is incomplete")
	}
	if len(claim.Secrets) != 0 {
		return resumedProgramAdmission{}, errors.New("resumed Program cannot receive new Secrets")
	}
	execution := claim.Execution
	switch {
	case execution.Restore != nil &&
		execution.Fresh == nil &&
		execution.Attach == nil:
		restore := execution.Restore
		admission := resumedProgramAdmission{
			runWaitID:            strings.TrimSpace(restore.RunWaitID),
			checkpointID:         strings.TrimSpace(restore.CheckpointID),
			resumeAttachID:       strings.TrimSpace(restore.ResumeAttachID),
			resumeRequestVersion: restore.ResumeRequestVersion,
			correlationID:        strings.TrimSpace(restore.CorrelationID),
			entrypointKind:       strings.TrimSpace(restore.EntrypointKind),
			entrypointDeclaredID: strings.TrimSpace(restore.EntrypointDeclaredID),
			decision:             restore.Decision,
			start: api.WorkerRunStartRequest{Restore: &api.WorkerRunStartRestore{
				RunWaitID:            restore.RunWaitID,
				CheckpointID:         restore.CheckpointID,
				ResumeAttachID:       restore.ResumeAttachID,
				ResumeRequestVersion: restore.ResumeRequestVersion,
			}},
			recreated: restore.Recreated != nil,
		}
		if admission.runWaitID == "" ||
			admission.checkpointID == "" ||
			admission.resumeAttachID == "" ||
			admission.resumeRequestVersion <= 0 ||
			admission.correlationID == "" ||
			admission.entrypointDeclaredID == "" ||
			((restore.Recreated == nil) == (restore.Retained == nil)) {
			return resumedProgramAdmission{}, errors.New(
				"Program restore authority is incomplete",
			)
		}
		if restore.Retained != nil &&
			strings.TrimSpace(restore.Retained.EnclosingRunWaitID) == "" {
			return resumedProgramAdmission{}, errors.New(
				"retained Program restore authority is incomplete",
			)
		}
		if restore.Recreated != nil {
			var checkpoint api.WorkerCheckpointManifest
			if err := json.Unmarshal(restore.Recreated.Manifest, &checkpoint); err != nil {
				return resumedProgramAdmission{}, fmt.Errorf(
					"decode restored Program Checkpoint: %w",
					err,
				)
			}
			if checkpoint.RecoveryPoint.ID != admission.checkpointID ||
				checkpoint.RecoveryPoint.RunID != lease.RunID ||
				checkpoint.RecoveryPoint.AttemptNumber != lease.AttemptNumber ||
				checkpoint.RecoveryPoint.RunWaitID != admission.runWaitID ||
				checkpoint.RecoveryPoint.CorrelationID != admission.correlationID {
				return resumedProgramAdmission{}, errors.New(
					"restored Program Checkpoint identity is inconsistent",
				)
			}
		}
		return admission, nil
	case execution.Restore == nil &&
		execution.Fresh == nil &&
		execution.Attach != nil &&
		execution.Attach.Parent != nil &&
		execution.Attach.Child == nil:
		parent := execution.Attach.Parent
		admission := resumedProgramAdmission{
			runWaitID:            strings.TrimSpace(parent.RunWaitID),
			checkpointID:         strings.TrimSpace(parent.CheckpointID),
			resumeAttachID:       strings.TrimSpace(parent.ResumeAttachID),
			resumeRequestVersion: parent.ResumeRequestVersion,
			correlationID:        strings.TrimSpace(parent.CorrelationID),
			entrypointKind:       strings.TrimSpace(parent.EntrypointKind),
			entrypointDeclaredID: strings.TrimSpace(parent.EntrypointDeclaredID),
			decision:             parent.Decision,
			start: api.WorkerRunStartRequest{Attach: &api.WorkerRunStartAttach{
				Parent: &api.WorkerRunStartParentAttach{
					RunWaitID:            parent.RunWaitID,
					CheckpointID:         parent.CheckpointID,
					ResumeAttachID:       parent.ResumeAttachID,
					ResumeRequestVersion: parent.ResumeRequestVersion,
				},
			}},
		}
		if admission.runWaitID == "" ||
			admission.checkpointID == "" ||
			admission.resumeAttachID == "" ||
			admission.resumeRequestVersion <= 0 ||
			admission.correlationID == "" ||
			admission.entrypointDeclaredID == "" {
			return resumedProgramAdmission{}, errors.New(
				"parent-attached Program authority is incomplete",
			)
		}
		return admission, nil
	default:
		return resumedProgramAdmission{}, errors.New(
			"Run Lease execution must contain exactly one restored or parent-attached Program",
		)
	}
}

func validateResumedProgramMount(
	lease api.WorkerRunLeaseReceipt,
	mount api.WorkerWorkspaceMount,
	resume resumedProgramAdmission,
) error {
	if mount.ID != lease.WorkspaceMountID ||
		mount.WorkspaceID != lease.WorkspaceID ||
		mount.RuntimeInstanceID != lease.RuntimeInstanceID ||
		mount.BaseVersionID != lease.BaseWorkspaceVersionID ||
		mount.FencingGeneration != lease.MountFencingGeneration ||
		(resume.recreated && mount.RestoreCheckpointID != resume.checkpointID) {
		return errors.New(
			"resumed Workspace mount does not match the claimed physical authority",
		)
	}
	return nil
}

func resumedEntrypoint(
	kind string,
	declaredID string,
) (*runv0.EntrypointIdentity, error) {
	switch kind {
	case "task":
		return &runv0.EntrypointIdentity{
			DeclaredId: declaredID,
			Kind: &runv0.EntrypointIdentity_Task{
				Task: &runv0.TaskEntrypoint{},
			},
		}, nil
	case "actor":
		return &runv0.EntrypointIdentity{
			DeclaredId: declaredID,
			Kind: &runv0.EntrypointIdentity_Actor{
				Actor: &runv0.ActorEntrypoint{},
			},
		}, nil
	default:
		return nil, errors.New("resumed Program entrypoint kind is unsupported")
	}
}

func restoredProgramDecision(decision api.WorkerRunLeaseDecision) (string, json.RawMessage, bool, error) {
	count := 0
	if decision.Completed != nil {
		count++
	}
	if decision.Failed != nil {
		count++
	}
	if decision.Cancelled != nil {
		count++
	}
	if count != 1 {
		return "", nil, false, errors.New("restored Program decision must contain exactly one terminal condition")
	}
	if completed := decision.Completed; completed != nil {
		if (completed.NoResult == nil) == (completed.ResultJSON == nil) {
			return "", nil, false, errors.New("completed restored Program decision must contain exactly one result variant")
		}
		if completed.NoResult != nil {
			return "completed", nil, true, nil
		}
		if completed.ResultJSON != nil {
			if !json.Valid(completed.ResultJSON) {
				return "", nil, false, errors.New("restored Program result is not valid JSON")
			}
			return "completed", append(json.RawMessage(nil), completed.ResultJSON...), false, nil
		}
	}
	if failed := decision.Failed; failed != nil {
		return restoredProgramFailureDecision("failed", failed.ReasonCode, failed.Error)
	}
	cancelled := decision.Cancelled
	return restoredProgramFailureDecision("cancelled", cancelled.ReasonCode, cancelled.Error)
}

func restoredProgramFailureDecision(kind, reason string, detail json.RawMessage) (string, json.RawMessage, bool, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", nil, false, errors.New("restored Program failure reason is required")
	}
	if detail != nil && !json.Valid(detail) {
		return "", nil, false, errors.New("restored Program failure detail is not valid JSON")
	}
	payload := struct {
		ReasonCode string          `json:"reason_code"`
		Error      json.RawMessage `json:"error,omitempty"`
	}{ReasonCode: reason, Error: detail}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", nil, false, fmt.Errorf("encode restored Program failure: %w", err)
	}
	return kind, encoded, false, nil
}
