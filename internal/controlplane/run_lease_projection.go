package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type runLeaseExecutionProjection struct {
	mode                runLeaseClaimMode
	run                 db.Run
	attempt             db.RunAttempt
	actor               *db.Session
	definition          db.DeploymentDefinition
	deploymentVersion   string
	runtime             db.RuntimeInstance
	workspaceMount      db.WorkspaceMount
	runWait             db.RunWait
	checkpoint          db.RunCheckpoint
	checkpointArtifacts checkpointArtifactAuthority
}

func projectRunLeaseExecution(
	authority runLeaseExecutionProjection,
) (workerapi.RunLeaseExecution, error) {
	switch authority.mode {
	case runLeaseClaimFresh:
		if authority.runWait.ID.Valid ||
			authority.checkpoint.ID.Valid ||
			!authority.checkpointArtifacts.empty() {
			return workerapi.RunLeaseExecution{}, errors.New("fresh run lease contains resume authority")
		}
		start, err := encodeProgramStart(
			authority.run,
			authority.attempt,
			authority.actor,
			authority.definition,
			authority.deploymentVersion,
		)
		if err != nil {
			return workerapi.RunLeaseExecution{}, err
		}
		return workerapi.RunLeaseExecution{
			Fresh: &workerapi.RunLeaseFresh{ProgramStart: start},
		}, nil
	case runLeaseClaimRestore:
		if !authority.runWait.ID.Valid ||
			!authority.checkpoint.ID.Valid ||
			!authority.attempt.EntrypointEnteredAt.Valid ||
			authority.runWait.ResumeRequestVersion <= 0 {
			return workerapi.RunLeaseExecution{}, errors.New("restore run lease authority is incomplete")
		}
		waitID, err := requiredClaimUUIDString("run wait ID", authority.runWait.ID)
		if err != nil {
			return workerapi.RunLeaseExecution{}, err
		}
		attachID, err := requiredClaimUUIDString("resume attach ID", authority.runWait.ResumeAttachID)
		if err != nil {
			return workerapi.RunLeaseExecution{}, err
		}
		decision, err := projectRunWaitDecision(authority.runWait)
		if err != nil {
			return workerapi.RunLeaseExecution{}, err
		}
		checkpointID, err := requiredClaimUUIDString("run checkpoint ID", authority.checkpoint.ID)
		if err != nil {
			return workerapi.RunLeaseExecution{}, err
		}
		correlationID, err := checkpointCorrelationID(
			authority.checkpoint,
			authority.runWait,
		)
		if err != nil {
			return workerapi.RunLeaseExecution{}, err
		}
		checkpoint, err := projectRunLeaseCheckpoint(authority.checkpoint, authority.checkpointArtifacts)
		if err != nil {
			return workerapi.RunLeaseExecution{}, err
		}
		restore := workerapi.RunLeaseRestore{
			RunWaitID:            waitID,
			CheckpointID:         checkpointID,
			ResumeAttachID:       attachID,
			ResumeRequestVersion: authority.runWait.ResumeRequestVersion,
			CorrelationID:        correlationID,
			EntrypointKind:       authority.run.EntrypointKind,
			EntrypointDeclaredID: authority.run.EntrypointDeclaredID,
			Manifest:             checkpoint.Manifest,
			Artifacts:            checkpoint.Artifacts,
			Decision:             decision,
		}
		return workerapi.RunLeaseExecution{
			Restore: &restore,
		}, nil
	default:
		return workerapi.RunLeaseExecution{}, fmt.Errorf(
			"run lease claim mode %q is unsupported",
			authority.mode,
		)
	}
}

func checkpointCorrelationID(
	checkpoint db.RunCheckpoint,
	wait db.RunWait,
) (string, error) {
	var manifest workerapi.CheckpointManifest
	if err := json.Unmarshal(checkpoint.RestoreManifest, &manifest); err != nil {
		return "", fmt.Errorf("decode run checkpoint correlation authority: %w", err)
	}
	correlationID := strings.TrimSpace(manifest.RecoveryPoint.CorrelationID)
	if correlationID == "" ||
		manifest.RecoveryPoint.ID != pgvalue.UUIDString(checkpoint.ID) ||
		manifest.RecoveryPoint.RunID != pgvalue.UUIDString(checkpoint.RunID) ||
		manifest.RecoveryPoint.AttemptNumber != checkpoint.AttemptNumber ||
		manifest.RecoveryPoint.RunWaitID != pgvalue.UUIDString(wait.ID) {
		return "", errors.New("run checkpoint correlation authority is inconsistent")
	}
	return correlationID, nil
}

func projectSecretDeliveries(materials []secret.DeliveryMaterial) ([]workerapi.SecretDelivery, error) {
	ordered := append([]secret.DeliveryMaterial(nil), materials...)
	slices.SortFunc(ordered, func(left, right secret.DeliveryMaterial) int {
		if compared := strings.Compare(left.PlacementKind, right.PlacementKind); compared != 0 {
			return compared
		}
		return strings.Compare(left.PlacementTarget, right.PlacementTarget)
	})
	deliveries := make([]workerapi.SecretDelivery, 0, len(ordered))
	for index, material := range ordered {
		if strings.TrimSpace(material.PlacementTarget) == "" {
			return nil, errors.New("secret placement target is required")
		}
		if index > 0 &&
			ordered[index-1].PlacementKind == material.PlacementKind &&
			ordered[index-1].PlacementTarget == material.PlacementTarget {
			return nil, errors.New("secret placement is duplicated")
		}
		delivery := workerapi.SecretDelivery{Value: append([]byte(nil), material.Value...)}
		switch material.PlacementKind {
		case "env":
			delivery.Env = &workerapi.SecretEnv{Name: material.PlacementTarget}
		case "file":
			delivery.File = &workerapi.SecretFile{Path: material.PlacementTarget}
		default:
			return nil, fmt.Errorf("secret placement kind %q is unsupported", material.PlacementKind)
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

type runLeaseProjectionAuthority struct {
	run            db.Run
	attempt        db.RunAttempt
	runtime        db.RuntimeInstance
	runLease       db.RunLease
	workspace      db.Workspace
	workspaceMount db.WorkspaceMount
	workspaceLease db.WorkspaceLease
}

func projectRunLeaseAssignment(authority runLeaseProjectionAuthority) (workerapi.RunLeaseAssignment, error) {
	lease := authority.runLease
	id, err := requiredClaimUUIDString("run lease ID", lease.ID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	runID, err := requiredClaimUUIDString("run ID", lease.RunID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	workerID, err := requiredClaimUUIDString("worker instance ID", lease.WorkerInstanceID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	runtimeID, err := requiredClaimUUIDString("runtime instance ID", lease.RuntimeInstanceID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	workspaceID, err := requiredClaimUUIDString("workspace ID", authority.workspace.ID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	mountID, err := requiredClaimUUIDString("workspace mount ID", authority.workspaceMount.ID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	workspaceLeaseID, err := requiredClaimUUIDString("workspace lease ID", authority.workspaceLease.ID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	baseWorkspaceVersionID, err := requiredClaimUUIDString(
		"base workspace version ID",
		authority.workspaceLease.BaseVersionID,
	)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	if lease.RunID != authority.run.ID ||
		lease.AttemptNumber != authority.attempt.Number ||
		lease.WorkspaceID != authority.workspace.ID ||
		lease.RuntimeInstanceID != authority.runtime.ID ||
		authority.workspaceMount.RuntimeInstanceID != lease.RuntimeInstanceID ||
		authority.workspaceLease.OwnerRunLeaseID != lease.ID ||
		authority.workspaceLease.RuntimeInstanceID != lease.RuntimeInstanceID ||
		authority.workspaceLease.WorkspaceMountID != authority.workspaceMount.ID ||
		authority.workspaceLease.WorkspaceID != lease.WorkspaceID ||
		authority.workspaceLease.BaseVersionID != authority.workspaceMount.MaterializedVersionID {
		return workerapi.RunLeaseAssignment{}, errors.New("run lease assignment authority is inconsistent")
	}
	if lease.AttemptNumber <= 0 ||
		lease.LeaseSequence <= 0 ||
		lease.WorkerEpoch <= 0 ||
		authority.workspaceLease.OwnershipGeneration <= 0 ||
		authority.workspaceLease.WriterGeneration <= 0 ||
		authority.workspaceLease.MountFencingGeneration <= 0 ||
		lease.RequestedCPUMillis <= 0 ||
		lease.RequestedMemoryBytes <= 0 ||
		lease.RequestedGuestEphemeralDiskBytes <= 0 ||
		lease.RequestedExecutionSlots <= 0 ||
		authority.run.MaxActiveDurationMs <= 0 ||
		authority.run.ActiveElapsedMs < 0 ||
		!lease.StartDeadlineAt.Valid ||
		!lease.ExpiresAt.Valid ||
		lease.StartDeadlineAt.Time.After(lease.ExpiresAt.Time) {
		return workerapi.RunLeaseAssignment{}, errors.New("run lease assignment fields are invalid")
	}
	if !lease.WorkerGroupID.Valid {
		return workerapi.RunLeaseAssignment{}, errors.New("worker group ID is required")
	}
	for name, value := range map[string]string{
		"runtime identity ID": lease.RuntimeIdentityID,
	} {
		if strings.TrimSpace(value) == "" {
			return workerapi.RunLeaseAssignment{}, fmt.Errorf("%s is required", name)
		}
	}
	return workerapi.RunLeaseAssignment{
		ID:                               id,
		RunID:                            runID,
		AttemptNumber:                    lease.AttemptNumber,
		LeaseSequence:                    lease.LeaseSequence,
		WorkerGroupID:                    pgvalue.UUIDString(lease.WorkerGroupID),
		WorkerInstanceID:                 workerID,
		WorkerEpoch:                      lease.WorkerEpoch,
		RuntimeInstanceID:                runtimeID,
		RuntimeIdentityID:                lease.RuntimeIdentityID,
		WorkspaceID:                      workspaceID,
		WorkspaceMountID:                 mountID,
		WorkspaceLeaseID:                 workspaceLeaseID,
		BaseWorkspaceVersionID:           baseWorkspaceVersionID,
		OwnershipGeneration:              authority.workspaceLease.OwnershipGeneration,
		WriterGeneration:                 authority.workspaceLease.WriterGeneration,
		MountFencingGeneration:           authority.workspaceLease.MountFencingGeneration,
		RequestedCPUMillis:               lease.RequestedCPUMillis,
		RequestedMemoryBytes:             lease.RequestedMemoryBytes,
		RequestedGuestEphemeralDiskBytes: lease.RequestedGuestEphemeralDiskBytes,
		RequestedExecutionSlots:          lease.RequestedExecutionSlots,
		MaxActiveDurationMs:              authority.run.MaxActiveDurationMs,
		ActiveElapsedMs:                  authority.run.ActiveElapsedMs,
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
) (workerapi.WorkspaceAttachment, error) {
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
		return workerapi.WorkspaceAttachment{}, errors.New("workspace attachment authority is inconsistent")
	}
	resetTarget, err := projectWorkspaceResetTarget(lease, resetAuthority)
	if err != nil {
		return workerapi.WorkspaceAttachment{}, err
	}
	return workerapi.WorkspaceAttachment{WriteCapability: writeCapability, ResetTarget: resetTarget}, nil
}

func projectWorkspaceResetTarget(
	lease db.WorkspaceLease,
	authority db.GetWorkspaceResetTargetAuthorityRow,
) (workerapi.WorkspaceResetTarget, error) {
	if authority.VersionID != lease.BaseVersionID {
		return workerapi.WorkspaceResetTarget{}, errors.New("workspace reset target does not match the workspace lease base")
	}
	baseVersionID, err := requiredClaimUUIDString("workspace reset base version ID", authority.VersionID)
	if err != nil {
		return workerapi.WorkspaceResetTarget{}, err
	}
	tree := workspace.TreeIdentity{
		Digest: authority.ContentDigest, SizeBytes: authority.LogicalSizeBytes,
		EntryCount: int(authority.EntryCount),
	}
	projectedTree := workerapi.WorkspaceTreeIdentity{
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
			return workerapi.WorkspaceResetTarget{}, fmt.Errorf("invalid empty workspace reset target authority: %w", err)
		}
		return workerapi.WorkspaceResetTarget{
			BaseWorkspaceVersionID: baseVersionID, Tree: projectedTree,
			Empty: &workerapi.EmptyWorkspace{},
		}, nil
	}
	artifactShape := authority.ParentVersionID.Valid && authority.ArtifactID.Valid &&
		authority.ArtifactKind.Valid && authority.ArtifactKind.ArtifactKind == db.ArtifactKindWorkspaceVersion &&
		authority.VersionKind == db.WorkspaceVersionKindUser && authority.SourceWorkspaceLeaseID.Valid &&
		authority.OwnershipGeneration > 0 && authority.WriterGeneration > 0 &&
		authority.ArtifactRowKind.Valid && authority.ArtifactRowKind.ArtifactKind == db.ArtifactKindWorkspaceVersion &&
		authority.ArtifactDigest.Valid && authority.ArtifactSizeBytes.Valid && authority.ArtifactMediaType.Valid
	if !artifactShape {
		return workerapi.WorkspaceResetTarget{}, errors.New("workspace reset target authority has an invalid version/artifact relation")
	}
	artifact := workspace.ArtifactIdentity{
		Digest: authority.ArtifactDigest.String, MediaType: authority.ArtifactMediaType.String,
		Encoding: workspace.ArtifactEncoding, SizeBytes: authority.ArtifactSizeBytes.Int64,
		EntryCount: int(authority.EntryCount),
	}
	if _, err := workspace.ArtifactResetTarget(baseVersionID, tree, artifact); err != nil {
		return workerapi.WorkspaceResetTarget{}, fmt.Errorf("invalid artifact workspace reset target authority: %w", err)
	}
	return workerapi.WorkspaceResetTarget{
		BaseWorkspaceVersionID: baseVersionID,
		Tree:                   projectedTree,
		Artifact: &workerapi.WorkspaceArtifact{
			Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
			SizeBytes: artifact.SizeBytes, EntryCount: authority.EntryCount,
		},
	}, nil
}

func projectRunWaitDecision(wait db.RunWait) (workerapi.RunLeaseDecision, error) {
	if !wait.ConditionTerminalAt.Valid {
		return workerapi.RunLeaseDecision{}, errors.New("terminal wait decision has no terminal timestamp")
	}
	switch wait.ConditionState {
	case db.WaitStateCompleted:
		if wait.ConditionReasonCode.Valid || wait.ConditionError != nil {
			return workerapi.RunLeaseDecision{}, errors.New("completed wait contains failure authority")
		}
		completed := &workerapi.RunLeaseCompleted{}
		if wait.ConditionResult == nil {
			completed.NoResult = &struct{}{}
		} else {
			if !json.Valid(wait.ConditionResult) {
				return workerapi.RunLeaseDecision{}, errors.New("completed wait result is not valid JSON")
			}
			completed.ResultJSON = append(json.RawMessage(nil), wait.ConditionResult...)
		}
		return workerapi.RunLeaseDecision{Completed: completed}, nil
	case db.WaitStateFailed:
		failed, err := projectRunLeaseFailure(wait)
		if err != nil {
			return workerapi.RunLeaseDecision{}, err
		}
		return workerapi.RunLeaseDecision{Failed: &workerapi.RunLeaseFailed{
			ReasonCode: failed.reason,
			Error:      failed.detail,
		}}, nil
	case db.WaitStateCancelled:
		failed, err := projectRunLeaseFailure(wait)
		if err != nil {
			return workerapi.RunLeaseDecision{}, err
		}
		return workerapi.RunLeaseDecision{Cancelled: &workerapi.RunLeaseCancelled{
			ReasonCode: failed.reason,
			Error:      failed.detail,
		}}, nil
	default:
		return workerapi.RunLeaseDecision{}, fmt.Errorf(
			"wait condition state %q is not terminal",
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
		return runLeaseFailure{}, errors.New("terminal wait reason is required")
	}
	var detail json.RawMessage
	if wait.ConditionError != nil {
		if !json.Valid(wait.ConditionError) {
			return runLeaseFailure{}, errors.New("terminal wait error is not valid JSON")
		}
		detail = append(json.RawMessage(nil), wait.ConditionError...)
	}
	if wait.ConditionResult != nil {
		return runLeaseFailure{}, errors.New("failed or cancelled wait contains a result")
	}
	return runLeaseFailure{reason: wait.ConditionReasonCode.String, detail: detail}, nil
}

type runLeaseCheckpointProjection struct {
	Manifest  json.RawMessage
	Artifacts []workerapi.RunLeaseCheckpointArtifact
}

type checkpointArtifactDescriptor struct {
	digest    string
	sizeBytes int64
	mediaType string
}

type checkpointArtifactAuthority struct {
	runtimeConfig checkpointArtifactDescriptor
	vmState       checkpointArtifactDescriptor
	memory        checkpointArtifactDescriptor
	scratchDisk   checkpointArtifactDescriptor
}

func (authority checkpointArtifactAuthority) empty() bool {
	return authority == (checkpointArtifactAuthority{})
}

func checkpointArtifactAuthorityFromReady(row db.GetReadyRunCheckpointRow) checkpointArtifactAuthority {
	return checkpointArtifactAuthority{
		runtimeConfig: checkpointArtifactDescriptor{
			digest: row.RuntimeConfigDigest, sizeBytes: row.RuntimeConfigSizeBytes, mediaType: row.RuntimeConfigMediaType,
		},
		vmState: checkpointArtifactDescriptor{
			digest: row.VMStateDigest, sizeBytes: row.VMStateSizeBytes, mediaType: row.VMStateMediaType,
		},
		memory: checkpointArtifactDescriptor{
			digest: row.MemoryDigest, sizeBytes: row.MemorySizeBytes, mediaType: row.MemoryMediaType,
		},
		scratchDisk: checkpointArtifactDescriptor{
			digest: row.ScratchDiskDigest, sizeBytes: row.ScratchDiskSizeBytes, mediaType: row.ScratchDiskMediaType,
		},
	}
}

func projectRunLeaseCheckpoint(
	checkpoint db.RunCheckpoint,
	authority checkpointArtifactAuthority,
) (runLeaseCheckpointProjection, error) {
	if checkpoint.State != db.RunCheckpointStateReady || !json.Valid(checkpoint.RestoreManifest) {
		return runLeaseCheckpointProjection{}, errors.New("run checkpoint authority is invalid")
	}
	descriptors := [4]struct {
		role       string
		descriptor checkpointArtifactDescriptor
	}{
		{role: "runtime_config", descriptor: authority.runtimeConfig},
		{role: "vm_state", descriptor: authority.vmState},
		{role: "memory", descriptor: authority.memory},
		{role: "scratch_disk", descriptor: authority.scratchDisk},
	}
	artifacts := make([]workerapi.RunLeaseCheckpointArtifact, 0, len(descriptors))
	for _, item := range descriptors {
		object, err := projectCASObject(
			item.descriptor.digest,
			item.descriptor.sizeBytes,
			item.descriptor.mediaType,
			"run checkpoint artifact",
		)
		if err != nil {
			return runLeaseCheckpointProjection{}, err
		}
		artifacts = append(artifacts, workerapi.RunLeaseCheckpointArtifact{
			Role: item.role, Ordinal: 0, Object: object,
		})
	}
	return runLeaseCheckpointProjection{
		Manifest:  append(json.RawMessage(nil), checkpoint.RestoreManifest...),
		Artifacts: artifacts,
	}, nil
}
