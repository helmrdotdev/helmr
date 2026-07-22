package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type runLeaseRestoreSource string

const (
	runLeaseRestoreRecreated runLeaseRestoreSource = "recreated"
	runLeaseRestoreRetained  runLeaseRestoreSource = "retained"
)

type runLeaseExecutionProjection struct {
	mode                runLeaseClaimMode
	restoreSource       runLeaseRestoreSource
	run                 db.Run
	attempt             db.RunAttempt
	actor               *db.Actor
	definition          db.DeploymentDefinition
	deploymentVersion   string
	runtime             db.RuntimeInstance
	workspaceMount      db.WorkspaceMount
	enclosingWait       db.RunWait
	runWait             db.RunWait
	checkpoint          db.RunCheckpoint
	childRun            db.Run
	checkpointArtifacts []db.ListRunCheckpointArtifactAuthorityRow
}

func projectRunLeaseExecution(
	authority runLeaseExecutionProjection,
) (api.WorkerRunLeaseExecution, error) {
	switch authority.mode {
	case runLeaseClaimFresh:
		if authority.runWait.ID.Valid ||
			authority.checkpoint.ID.Valid ||
			authority.childRun.ID.Valid ||
			authority.runtime.RestoreCheckpointID.Valid ||
			len(authority.checkpointArtifacts) != 0 {
			return api.WorkerRunLeaseExecution{}, errors.New("fresh Run Lease contains resume authority")
		}
		start, err := encodeProgramStart(
			authority.run,
			authority.attempt,
			authority.actor,
			authority.definition,
			authority.deploymentVersion,
		)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		return api.WorkerRunLeaseExecution{
			Fresh: &api.WorkerRunLeaseFresh{ProgramStart: start},
		}, nil
	case runLeaseClaimRestore:
		if authority.childRun.ID.Valid ||
			!authority.runWait.ID.Valid ||
			!authority.checkpoint.ID.Valid ||
			!authority.attempt.EntrypointEnteredAt.Valid ||
			authority.runWait.ResumeRequestVersion <= 0 {
			return api.WorkerRunLeaseExecution{}, errors.New("restore Run Lease authority is incomplete")
		}
		waitID, err := requiredClaimUUIDString("Run Wait ID", authority.runWait.ID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		attachID, err := requiredClaimUUIDString("resume attach ID", authority.runWait.ResumeAttachID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		decision, err := projectRunWaitDecision(authority.runWait)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		checkpointID, err := requiredClaimUUIDString("Run Checkpoint ID", authority.checkpoint.ID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		restore := api.WorkerRunLeaseRestore{
			RunWaitID:            waitID,
			CheckpointID:         checkpointID,
			ResumeAttachID:       attachID,
			ResumeRequestVersion: authority.runWait.ResumeRequestVersion,
			Decision:             decision,
		}
		switch authority.restoreSource {
		case runLeaseRestoreRetained:
			if len(authority.checkpointArtifacts) != 0 ||
				authority.enclosingWait.HandoffRuntimeInstanceID != authority.runtime.ID ||
				authority.enclosingWait.HandoffWorkspaceMountID != authority.workspaceMount.ID ||
				!authority.enclosingWait.HandoffMountGeneration.Valid ||
				authority.enclosingWait.HandoffMountGeneration.Int64 != authority.workspaceMount.FencingGeneration {
				return api.WorkerRunLeaseExecution{}, errors.New("retained restore authority is incomplete")
			}
			enclosingWaitID, err := requiredClaimUUIDString("enclosing Run Wait ID", authority.enclosingWait.ID)
			if err != nil {
				return api.WorkerRunLeaseExecution{}, err
			}
			restore.Retained = &api.WorkerRunLeaseRetainedRestore{
				EnclosingRunWaitID: enclosingWaitID,
			}
		case runLeaseRestoreRecreated:
			if authority.runtime.RestoreCheckpointID != authority.checkpoint.ID {
				return api.WorkerRunLeaseExecution{}, errors.New("recreated restore runtime provenance is incomplete")
			}
			checkpoint, err := projectRunLeaseCheckpoint(
				authority.checkpoint,
				authority.checkpointArtifacts,
			)
			if err != nil {
				return api.WorkerRunLeaseExecution{}, err
			}
			restore.Recreated = &checkpoint
		default:
			return api.WorkerRunLeaseExecution{}, errors.New("restore source is required")
		}
		return api.WorkerRunLeaseExecution{
			Restore: &restore,
		}, nil
	case runLeaseClaimAttachChild:
		if !authority.runWait.ID.Valid ||
			!authority.checkpoint.ID.Valid ||
			authority.childRun.ID.Valid ||
			authority.attempt.EntrypointEnteredAt.Valid ||
			len(authority.checkpointArtifacts) != 0 {
			return api.WorkerRunLeaseExecution{}, errors.New("child attach authority is incomplete")
		}
		waitID, err := requiredClaimUUIDString("Run Wait ID", authority.runWait.ID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		checkpointID, err := requiredClaimUUIDString("Run Checkpoint ID", authority.checkpoint.ID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		attachID, err := requiredClaimUUIDString("resume attach ID", authority.runWait.ResumeAttachID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		start, err := encodeProgramStart(
			authority.run,
			authority.attempt,
			authority.actor,
			authority.definition,
			authority.deploymentVersion,
		)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		return api.WorkerRunLeaseExecution{
			Attach: &api.WorkerRunLeaseAttach{
				Child: &api.WorkerRunLeaseChildAttach{
					RunWaitID:      waitID,
					CheckpointID:   checkpointID,
					ResumeAttachID: attachID,
					ProgramStart:   start,
				},
			},
		}, nil
	case runLeaseClaimAttachParent:
		if !authority.runWait.ID.Valid ||
			!authority.checkpoint.ID.Valid ||
			!authority.childRun.ID.Valid ||
			!authority.attempt.EntrypointEnteredAt.Valid ||
			authority.runWait.ResumeRequestVersion <= 0 ||
			len(authority.checkpointArtifacts) != 0 {
			return api.WorkerRunLeaseExecution{}, errors.New("parent attach authority is incomplete")
		}
		waitID, err := requiredClaimUUIDString("Run Wait ID", authority.runWait.ID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		checkpointID, err := requiredClaimUUIDString("Run Checkpoint ID", authority.checkpoint.ID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		attachID, err := requiredClaimUUIDString("resume attach ID", authority.runWait.ResumeAttachID)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		decision, err := projectRunWaitDecision(authority.runWait)
		if err != nil {
			return api.WorkerRunLeaseExecution{}, err
		}
		return api.WorkerRunLeaseExecution{
			Attach: &api.WorkerRunLeaseAttach{
				Parent: &api.WorkerRunLeaseParentAttach{
					RunWaitID:            waitID,
					CheckpointID:         checkpointID,
					ResumeAttachID:       attachID,
					ResumeRequestVersion: authority.runWait.ResumeRequestVersion,
					Decision:             decision,
				},
			},
		}, nil
	default:
		return api.WorkerRunLeaseExecution{}, fmt.Errorf(
			"Run Lease claim mode %q is unsupported",
			authority.mode,
		)
	}
}

func projectSecretDeliveries(materials []secret.DeliveryMaterial) ([]api.WorkerSecretDelivery, error) {
	ordered := append([]secret.DeliveryMaterial(nil), materials...)
	slices.SortFunc(ordered, func(left, right secret.DeliveryMaterial) int {
		if compared := strings.Compare(left.PlacementKind, right.PlacementKind); compared != 0 {
			return compared
		}
		return strings.Compare(left.PlacementTarget, right.PlacementTarget)
	})
	deliveries := make([]api.WorkerSecretDelivery, 0, len(ordered))
	for index, material := range ordered {
		if strings.TrimSpace(material.PlacementTarget) == "" {
			return nil, errors.New("Secret placement target is required")
		}
		if index > 0 &&
			ordered[index-1].PlacementKind == material.PlacementKind &&
			ordered[index-1].PlacementTarget == material.PlacementTarget {
			return nil, errors.New("Secret placement is duplicated")
		}
		delivery := api.WorkerSecretDelivery{Value: append([]byte(nil), material.Value...)}
		switch material.PlacementKind {
		case "env":
			delivery.Env = &api.WorkerSecretEnv{Name: material.PlacementTarget}
		case "file":
			delivery.File = &api.WorkerSecretFile{Path: material.PlacementTarget}
		default:
			return nil, fmt.Errorf("Secret placement kind %q is unsupported", material.PlacementKind)
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

type runLeaseProjectionAuthority struct {
	run            db.Run
	attempt        db.RunAttempt
	runtime        db.RuntimeInstance
	networkSlot    db.WorkerNetworkSlot
	runLease       db.RunLease
	workspace      db.Workspace
	workspaceMount db.WorkspaceMount
	workspaceLease db.WorkspaceLease
}

func projectRunLeaseReceipt(authority runLeaseProjectionAuthority) (api.WorkerRunLeaseReceipt, error) {
	lease := authority.runLease
	id, err := requiredClaimUUIDString("Run Lease ID", lease.ID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	runID, err := requiredClaimUUIDString("Run ID", lease.RunID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	workerID, err := requiredClaimUUIDString("worker instance ID", lease.WorkerInstanceID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	runtimeID, err := requiredClaimUUIDString("runtime instance ID", lease.RuntimeInstanceID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	networkSlotID, err := requiredClaimUUIDString("network slot ID", lease.NetworkSlotID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	workspaceID, err := requiredClaimUUIDString("Workspace ID", authority.workspace.ID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	mountID, err := requiredClaimUUIDString("Workspace Mount ID", authority.workspaceMount.ID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	workspaceLeaseID, err := requiredClaimUUIDString("Workspace Lease ID", authority.workspaceLease.ID)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	baseWorkspaceVersionID, err := requiredClaimUUIDString(
		"base Workspace version ID",
		authority.workspaceLease.BaseVersionID,
	)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	if lease.RunID != authority.run.ID ||
		lease.AttemptNumber != authority.attempt.Number ||
		lease.WorkspaceID != authority.workspace.ID ||
		lease.RuntimeInstanceID != authority.runtime.ID ||
		lease.NetworkSlotID != authority.networkSlot.ID ||
		authority.workspaceMount.RuntimeInstanceID != lease.RuntimeInstanceID ||
		authority.workspaceLease.OwnerRunLeaseID != lease.ID ||
		authority.workspaceLease.RuntimeInstanceID != lease.RuntimeInstanceID ||
		authority.workspaceLease.WorkspaceMountID != authority.workspaceMount.ID ||
		authority.workspaceLease.WorkspaceID != lease.WorkspaceID ||
		authority.workspaceLease.BaseVersionID != authority.workspaceMount.MaterializedVersionID {
		return api.WorkerRunLeaseReceipt{}, errors.New("Run Lease receipt authority is inconsistent")
	}
	if lease.AttemptNumber <= 0 ||
		lease.LeaseSequence <= 0 ||
		lease.WorkerEpoch <= 0 ||
		lease.NetworkSlotGeneration <= 0 ||
		authority.workspaceLease.OwnershipGeneration <= 0 ||
		authority.workspaceLease.WriterGeneration <= 0 ||
		authority.workspaceLease.MountFencingGeneration <= 0 ||
		lease.RequestedCpuMillis <= 0 ||
		lease.RequestedMemoryBytes <= 0 ||
		lease.RequestedWorkloadDiskBytes <= 0 ||
		lease.RequestedScratchBytes < 0 ||
		lease.RequestedExecutionSlots <= 0 ||
		authority.run.MaxActiveDurationMs <= 0 ||
		authority.run.ActiveElapsedMs < 0 ||
		!lease.StartDeadlineAt.Valid ||
		!lease.ExpiresAt.Valid ||
		lease.StartDeadlineAt.Time.After(lease.ExpiresAt.Time) {
		return api.WorkerRunLeaseReceipt{}, errors.New("Run Lease receipt fields are invalid")
	}
	for name, value := range map[string]string{
		"worker group ID":         lease.WorkerGroupID,
		"worker protocol version": lease.WorkerProtocolVersion,
		"runtime identity ID":     lease.RuntimeIdentityID,
	} {
		if strings.TrimSpace(value) == "" {
			return api.WorkerRunLeaseReceipt{}, fmt.Errorf("%s is required", name)
		}
	}
	return api.WorkerRunLeaseReceipt{
		ID:                         id,
		RunID:                      runID,
		AttemptNumber:              lease.AttemptNumber,
		LeaseSequence:              lease.LeaseSequence,
		WorkerGroupID:              lease.WorkerGroupID,
		WorkerInstanceID:           workerID,
		WorkerEpoch:                lease.WorkerEpoch,
		WorkerProtocolVersion:      lease.WorkerProtocolVersion,
		RuntimeInstanceID:          runtimeID,
		RuntimeIdentityID:          lease.RuntimeIdentityID,
		NetworkSlotID:              networkSlotID,
		NetworkSlotGeneration:      lease.NetworkSlotGeneration,
		WorkspaceID:                workspaceID,
		WorkspaceMountID:           mountID,
		WorkspaceLeaseID:           workspaceLeaseID,
		BaseWorkspaceVersionID:     baseWorkspaceVersionID,
		OwnershipGeneration:        authority.workspaceLease.OwnershipGeneration,
		WriterGeneration:           authority.workspaceLease.WriterGeneration,
		MountFencingGeneration:     authority.workspaceLease.MountFencingGeneration,
		RequestedCPUMillis:         lease.RequestedCpuMillis,
		RequestedMemoryBytes:       lease.RequestedMemoryBytes,
		RequestedWorkloadDiskBytes: lease.RequestedWorkloadDiskBytes,
		RequestedScratchBytes:      lease.RequestedScratchBytes,
		RequestedExecutionSlots:    lease.RequestedExecutionSlots,
		MaxActiveDurationMs:        authority.run.MaxActiveDurationMs,
		ActiveElapsedMs:            authority.run.ActiveElapsedMs,
		Trace: api.TraceContext{
			TraceID:     lease.TraceID.String,
			SpanID:      lease.SpanID.String,
			Traceparent: lease.Traceparent.String,
		},
		StartDeadlineAt: lease.StartDeadlineAt.Time,
		ExpiresAt:       lease.ExpiresAt.Time,
	}, nil
}

func projectWorkspaceAttachment(
	authority runLeaseProjectionAuthority,
	writeCapability string,
	resetAuthority db.GetWorkspaceResetTargetAuthorityRow,
) (api.WorkerWorkspaceAttachment, error) {
	lease := authority.workspaceLease
	if lease.OwnerRunLeaseID != authority.runLease.ID ||
		lease.WorkspaceID != authority.workspace.ID ||
		lease.RuntimeInstanceID != authority.runtime.ID ||
		lease.WorkspaceMountID != authority.workspaceMount.ID ||
		lease.BaseVersionID != authority.workspaceMount.MaterializedVersionID ||
		lease.OwnershipGeneration != authority.workspace.OwnershipGeneration ||
		lease.WriterGeneration != authority.workspace.WriterGeneration ||
		lease.MountFencingGeneration != authority.workspaceMount.FencingGeneration ||
		lease.OwnershipGeneration <= 0 ||
		lease.WriterGeneration <= 0 ||
		lease.MountFencingGeneration <= 0 ||
		strings.TrimSpace(writeCapability) == "" {
		return api.WorkerWorkspaceAttachment{}, errors.New("Workspace attachment authority is inconsistent")
	}
	resetTarget, err := projectWorkspaceResetTarget(lease, resetAuthority)
	if err != nil {
		return api.WorkerWorkspaceAttachment{}, err
	}
	return api.WorkerWorkspaceAttachment{WriteCapability: writeCapability, ResetTarget: resetTarget}, nil
}

func projectWorkspaceResetTarget(
	lease db.WorkspaceLease,
	authority db.GetWorkspaceResetTargetAuthorityRow,
) (api.WorkerWorkspaceResetTarget, error) {
	if authority.VersionID != lease.BaseVersionID {
		return api.WorkerWorkspaceResetTarget{}, errors.New("Workspace Reset target does not match the Workspace Lease base")
	}
	baseVersionID, err := requiredClaimUUIDString("Workspace Reset base version ID", authority.VersionID)
	if err != nil {
		return api.WorkerWorkspaceResetTarget{}, err
	}
	tree := workspace.TreeIdentity{
		Digest: authority.ContentDigest, SizeBytes: authority.LogicalSizeBytes,
		EntryCount: int(authority.EntryCount),
	}
	projectedTree := api.WorkerWorkspaceTreeIdentity{
		Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: authority.EntryCount,
	}
	emptyShape := !authority.ParentVersionID.Valid && !authority.ArtifactID.Valid &&
		!authority.ArtifactKind.Valid && authority.VersionKind == db.WorkspaceVersionKindSystem &&
		!authority.SourceWorkspaceLeaseID.Valid && authority.OwnershipGeneration == 0 &&
		authority.WriterGeneration == 0 && !authority.ArtifactRowKind.Valid &&
		!authority.ArtifactDigest.Valid && !authority.ArtifactSizeBytes.Valid &&
		!authority.ArtifactMediaType.Valid
	if emptyShape {
		if _, err := workspace.EmptyResetTarget(baseVersionID, tree); err != nil {
			return api.WorkerWorkspaceResetTarget{}, fmt.Errorf("invalid empty Workspace Reset target authority: %w", err)
		}
		return api.WorkerWorkspaceResetTarget{
			BaseWorkspaceVersionID: baseVersionID, Tree: projectedTree,
			Empty: &api.WorkerEmptyWorkspace{},
		}, nil
	}
	artifactShape := authority.ParentVersionID.Valid && authority.ArtifactID.Valid &&
		authority.ArtifactKind.Valid && authority.ArtifactKind.ArtifactKind == db.ArtifactKindWorkspaceVersion &&
		authority.VersionKind == db.WorkspaceVersionKindUser && authority.SourceWorkspaceLeaseID.Valid &&
		authority.OwnershipGeneration > 0 && authority.WriterGeneration > 0 &&
		authority.ArtifactRowKind.Valid && authority.ArtifactRowKind.ArtifactKind == db.ArtifactKindWorkspaceVersion &&
		authority.ArtifactDigest.Valid && authority.ArtifactSizeBytes.Valid && authority.ArtifactMediaType.Valid
	if !artifactShape {
		return api.WorkerWorkspaceResetTarget{}, errors.New("Workspace Reset target authority has an invalid version/Artifact relation")
	}
	artifact := workspace.ArtifactIdentity{
		Digest: authority.ArtifactDigest.String, MediaType: authority.ArtifactMediaType.String,
		Encoding: workspace.ArtifactEncoding, SizeBytes: authority.ArtifactSizeBytes.Int64,
		EntryCount: int(authority.EntryCount),
	}
	if _, err := workspace.ArtifactResetTarget(baseVersionID, tree, artifact); err != nil {
		return api.WorkerWorkspaceResetTarget{}, fmt.Errorf("invalid Artifact Workspace Reset target authority: %w", err)
	}
	return api.WorkerWorkspaceResetTarget{
		BaseWorkspaceVersionID: baseVersionID,
		Tree:                   projectedTree,
		Artifact: &api.WorkerWorkspaceArtifact{
			Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
			SizeBytes: artifact.SizeBytes, EntryCount: authority.EntryCount,
		},
	}, nil
}

func projectRunWaitDecision(wait db.RunWait) (api.WorkerRunLeaseDecision, error) {
	if !wait.ConditionTerminalAt.Valid {
		return api.WorkerRunLeaseDecision{}, errors.New("terminal Wait decision has no terminal timestamp")
	}
	switch wait.ConditionState {
	case db.WaitStateCompleted:
		if wait.ConditionReasonCode.Valid || wait.ConditionError != nil {
			return api.WorkerRunLeaseDecision{}, errors.New("completed Wait contains failure authority")
		}
		completed := &api.WorkerRunLeaseCompleted{}
		if wait.ConditionResult == nil {
			completed.NoResult = &struct{}{}
		} else {
			if !json.Valid(wait.ConditionResult) {
				return api.WorkerRunLeaseDecision{}, errors.New("completed Wait result is not valid JSON")
			}
			completed.ResultJSON = append(json.RawMessage(nil), wait.ConditionResult...)
		}
		return api.WorkerRunLeaseDecision{Completed: completed}, nil
	case db.WaitStateFailed:
		failed, err := projectRunLeaseFailure(wait)
		if err != nil {
			return api.WorkerRunLeaseDecision{}, err
		}
		return api.WorkerRunLeaseDecision{Failed: &api.WorkerRunLeaseFailed{
			ReasonCode: failed.reason,
			Error:      failed.detail,
		}}, nil
	case db.WaitStateCancelled:
		failed, err := projectRunLeaseFailure(wait)
		if err != nil {
			return api.WorkerRunLeaseDecision{}, err
		}
		return api.WorkerRunLeaseDecision{Cancelled: &api.WorkerRunLeaseCancelled{
			ReasonCode: failed.reason,
			Error:      failed.detail,
		}}, nil
	default:
		return api.WorkerRunLeaseDecision{}, fmt.Errorf(
			"Wait condition state %q is not terminal",
			wait.ConditionState,
		)
	}
}

type runLeaseFailure struct {
	reason string
	detail json.RawMessage
}

func projectRunLeaseFailure(wait db.RunWait) (runLeaseFailure, error) {
	if !wait.ConditionReasonCode.Valid || strings.TrimSpace(wait.ConditionReasonCode.String) == "" {
		return runLeaseFailure{}, errors.New("terminal Wait reason is required")
	}
	var detail json.RawMessage
	if wait.ConditionError != nil {
		if !json.Valid(wait.ConditionError) {
			return runLeaseFailure{}, errors.New("terminal Wait error is not valid JSON")
		}
		detail = append(json.RawMessage(nil), wait.ConditionError...)
	}
	if wait.ConditionResult != nil {
		return runLeaseFailure{}, errors.New("failed or cancelled Wait contains a result")
	}
	return runLeaseFailure{reason: wait.ConditionReasonCode.String, detail: detail}, nil
}

func projectRunLeaseCheckpoint(
	checkpoint db.RunCheckpoint,
	rows []db.ListRunCheckpointArtifactAuthorityRow,
) (api.WorkerRunLeaseRecreatedRestore, error) {
	if checkpoint.State != db.RunCheckpointStateReady ||
		(checkpoint.Kind != db.RunCheckpointKindSuspend &&
			checkpoint.Kind != db.RunCheckpointKindHandoffResume) ||
		!json.Valid(checkpoint.RestoreManifest) {
		return api.WorkerRunLeaseRecreatedRestore{}, errors.New("Run Checkpoint authority is invalid")
	}
	artifacts := make([]api.WorkerRunLeaseCheckpointArtifact, 0, len(rows))
	priorRank := -1
	var priorOrdinal int32
	for index, row := range rows {
		role := string(row.Role)
		rank, ok := checkpointArtifactRoleRank(row.Role)
		if !ok || row.Ordinal < 0 {
			return api.WorkerRunLeaseRecreatedRestore{}, errors.New("Run Checkpoint Artifact membership is invalid")
		}
		if index > 0 &&
			(rank < priorRank || (rank == priorRank && row.Ordinal <= priorOrdinal)) {
			return api.WorkerRunLeaseRecreatedRestore{}, errors.New("Run Checkpoint Artifact membership is not canonically ordered")
		}
		object, err := projectCASObject(
			row.Digest,
			row.SizeBytes,
			row.MediaType,
			"Run Checkpoint Artifact",
		)
		if err != nil {
			return api.WorkerRunLeaseRecreatedRestore{}, err
		}
		artifacts = append(artifacts, api.WorkerRunLeaseCheckpointArtifact{
			Role: role, Ordinal: row.Ordinal, Object: object,
		})
		priorRank = rank
		priorOrdinal = row.Ordinal
	}
	return api.WorkerRunLeaseRecreatedRestore{
		Kind:      string(checkpoint.Kind),
		Manifest:  append(json.RawMessage(nil), checkpoint.RestoreManifest...),
		Artifacts: artifacts,
	}, nil
}

func checkpointArtifactRoleRank(role db.RunCheckpointArtifactRole) (int, bool) {
	switch role {
	case db.RunCheckpointArtifactRoleRuntimeConfig:
		return 0, true
	case db.RunCheckpointArtifactRoleVmState:
		return 1, true
	case db.RunCheckpointArtifactRoleMemory:
		return 2, true
	case db.RunCheckpointArtifactRoleScratchDisk:
		return 3, true
	default:
		return 0, false
	}
}
