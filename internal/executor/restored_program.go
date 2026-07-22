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

func (r GuestRunner) startRestoredProgram(
	ctx context.Context,
	claim *api.WorkerRunLeaseClaimResponse,
	control RunLeaseControl,
) (freshProgram, error) {
	restore, checkpoint, err := validateRestoredProgramClaim(claim)
	if err != nil {
		return freshProgram{}, err
	}
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
	if opened.Mount.ID != claim.Lease.WorkspaceMountID ||
		opened.Mount.WorkspaceID != claim.Lease.WorkspaceID ||
		opened.Mount.RuntimeInstanceID != claim.Lease.RuntimeInstanceID ||
		opened.Mount.BaseVersionID != claim.Lease.BaseWorkspaceVersionID ||
		opened.Mount.FencingGeneration != claim.Lease.MountFencingGeneration ||
		opened.Mount.RestoreCheckpointID != restore.CheckpointID {
		return freshProgram{}, errors.New("restored Workspace mount does not match the claimed physical authority")
	}
	authority := freshWorkspaceAuthority(claim, opened.ChannelToken)
	correlationID := strings.TrimSpace(checkpoint.RecoveryPoint.CorrelationID)
	grant := &workspacev0.GrantProgramResumeRequest{
		Authority: authority, RunWaitId: restore.RunWaitID, CheckpointId: restore.CheckpointID,
		ResumeAttachId: restore.ResumeAttachID, ResumeRequestVersion: restore.ResumeRequestVersion,
		CorrelationId: correlationID,
	}
	if err := grantProgramResumeOnSession(admissionCtx, opened.ControlSession, grant); err != nil {
		return freshProgram{}, fmt.Errorf("install restored Program authority: %w", err)
	}
	var startResponse api.WorkerRunStartResponse
	if err := retryRunLeaseRequest(admissionCtx, func(requestCtx context.Context) error {
		var requestErr error
		startResponse, requestErr = control.AcknowledgeRunStart(requestCtx, api.WorkerRunStartRequest{
			Lease: claim.Lease,
			Restore: &api.WorkerRunStartRestore{
				RunWaitID: restore.RunWaitID, CheckpointID: restore.CheckpointID,
				ResumeAttachID: restore.ResumeAttachID, ResumeRequestVersion: restore.ResumeRequestVersion,
			},
		})
		return requestErr
	}); err != nil {
		return freshProgram{}, fmt.Errorf("acknowledge restored Run start: %w", err)
	}
	if !equalRunLeaseReceipt(startResponse.Lease, claim.Lease) {
		return freshProgram{}, errors.New("restored Run start acknowledgement changed the Run Lease receipt")
	}
	attach := &runv0.ResumeAttach{
		RunId: claim.Lease.RunID, AttemptNumber: uint32(claim.Lease.AttemptNumber),
		RunLeaseId: claim.Lease.ID, RunWaitId: restore.RunWaitID, CheckpointId: restore.CheckpointID,
		ResumeAttachId: restore.ResumeAttachID, ResumeRequestVersion: restore.ResumeRequestVersion,
		CorrelationId: correlationID,
	}
	if err := frameio.WriteProtoFrame(opened.Session.Stream(), attach); err != nil {
		return freshProgram{}, fmt.Errorf("attach restored Program: %w", err)
	}
	kind, data, noResult, err := restoredProgramDecision(restore.Decision)
	if err != nil {
		return freshProgram{}, err
	}
	decision := &runv0.ResumeDecision{
		RunWaitId: restore.RunWaitID, Kind: kind, DataJson: string(data), RequireConsumedAck: true,
		CheckpointId: restore.CheckpointID, ResumeAttachId: restore.ResumeAttachID,
		ResumeRequestVersion: restore.ResumeRequestVersion, RunLeaseId: claim.Lease.ID,
		CorrelationId: correlationID, NoResult: noResult,
	}
	if err := frameio.WriteProtoFrame(opened.Session.Stream(), decision); err != nil {
		return freshProgram{}, fmt.Errorf("apply restored Program decision: %w", err)
	}
	ackCtx, cancelAck := context.WithTimeout(admissionCtx, restoreAttachTimeout)
	ack, err := readResumeAck(ackCtx, opened.Session)
	cancelAck()
	if err != nil {
		return freshProgram{}, fmt.Errorf("read restored Program proof: %w", err)
	}
	if ack.GetRunWaitId() != restore.RunWaitID || ack.GetCheckpointId() != restore.CheckpointID ||
		ack.GetResumeAttachId() != restore.ResumeAttachID ||
		ack.GetResumeRequestVersion() != restore.ResumeRequestVersion ||
		ack.GetRunLeaseId() != claim.Lease.ID || ack.GetCorrelationId() != correlationID {
		return freshProgram{}, errors.New("restored Program proof did not match exact authority")
	}
	release := RestoreAcknowledgement{
		Lease: claim.Lease, RunWaitID: restore.RunWaitID, CheckpointID: restore.CheckpointID,
		ResumeAttachID: restore.ResumeAttachID, ResumeRequestVersion: restore.ResumeRequestVersion,
		CorrelationID: correlationID,
	}
	if err := retryRunLeaseRequest(admissionCtx, func(requestCtx context.Context) error {
		return (ControlRunWaits{Client: waitClient}).AcknowledgeRestore(requestCtx, release)
	}); err != nil {
		return freshProgram{}, fmt.Errorf("release restored Run Wait: %w", err)
	}
	keepSession = true
	return freshProgram{
		session: opened.Session, mount: opened.Mount, lease: claim.Lease, authority: authority,
		entrypoint: &runv0.EntrypointIdentity{DeclaredId: "restored-task", Kind: &runv0.EntrypointIdentity_Task{Task: &runv0.TaskEntrypoint{}}},
	}, nil
}

func validateRestoredProgramClaim(
	claim *api.WorkerRunLeaseClaimResponse,
) (*api.WorkerRunLeaseRestore, api.WorkerCheckpointManifest, error) {
	if claim == nil || claim.Execution.Restore == nil || claim.Execution.Fresh != nil || claim.Execution.Attach != nil {
		return nil, api.WorkerCheckpointManifest{}, errors.New("Run Lease execution must contain exactly one restored Program")
	}
	lease := claim.Lease
	restore := claim.Execution.Restore
	if strings.TrimSpace(lease.ID) == "" || strings.TrimSpace(lease.RunID) == "" || lease.AttemptNumber <= 0 ||
		lease.LeaseSequence <= 0 || strings.TrimSpace(lease.RuntimeInstanceID) == "" ||
		strings.TrimSpace(lease.RuntimeIdentityID) == "" || strings.TrimSpace(lease.WorkspaceID) == "" ||
		strings.TrimSpace(lease.WorkspaceMountID) == "" || strings.TrimSpace(lease.WorkspaceLeaseID) == "" ||
		strings.TrimSpace(lease.BaseWorkspaceVersionID) == "" || lease.MountFencingGeneration <= 0 ||
		lease.StartDeadlineAt.IsZero() ||
		lease.ExpiresAt.IsZero() || !lease.ExpiresAt.After(time.Now()) ||
		strings.TrimSpace(claim.Workspace.WriteCapability) == "" {
		return nil, api.WorkerCheckpointManifest{}, errors.New("restored Program Run Lease receipt is incomplete")
	}
	if restore.Recreated == nil || restore.Retained != nil || strings.TrimSpace(restore.RunWaitID) == "" ||
		strings.TrimSpace(restore.CheckpointID) == "" || strings.TrimSpace(restore.ResumeAttachID) == "" ||
		restore.ResumeRequestVersion <= 0 {
		return nil, api.WorkerCheckpointManifest{}, errors.New("recreated Program restore authority is incomplete")
	}
	var checkpoint api.WorkerCheckpointManifest
	if err := json.Unmarshal(restore.Recreated.Manifest, &checkpoint); err != nil {
		return nil, api.WorkerCheckpointManifest{}, fmt.Errorf("decode restored Program Checkpoint: %w", err)
	}
	if checkpoint.RecoveryPoint.ID != restore.CheckpointID ||
		checkpoint.RecoveryPoint.RunID != lease.RunID ||
		checkpoint.RecoveryPoint.AttemptNumber != lease.AttemptNumber ||
		checkpoint.RecoveryPoint.RunWaitID != restore.RunWaitID ||
		strings.TrimSpace(checkpoint.RecoveryPoint.CorrelationID) == "" {
		return nil, api.WorkerCheckpointManifest{}, errors.New("restored Program Checkpoint identity is inconsistent")
	}
	return restore, checkpoint, nil
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
