package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

const (
	maxTaskCompletionOutputBytes  = 16 << 20
	maxTaskCompletionErrorBytes   = 16 << 10
	maxTaskCompletionMessageBytes = 1024
)

var taskWorkspaceDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type taskCompletionKind string

const (
	taskCompletionSucceeded      taskCompletionKind = "succeeded"
	taskCompletionFailed         taskCompletionKind = "failed"
	taskCompletionPayloadInvalid taskCompletionKind = "payload_invalid"
)

type parsedTaskCompletion struct {
	lease       parsedRunLeaseFence
	kind        taskCompletionKind
	output      json.RawMessage
	errorObject json.RawMessage
	capture     *parsedTaskWorkspaceCapture
	rollback    *parsedTaskWorkspaceRollback
	handoff     *parsedTaskHandoffCheckpoint
	fingerprint string
}

type parsedTaskHandoffCheckpoint struct {
	checkpointID  uuid.UUID
	parentRunID   uuid.UUID
	waitID        uuid.UUID
	attemptNumber int32
	manifest      []byte
	artifacts     []checkpointArtifactProof
}

type parsedTaskWorkspaceCapture struct {
	receipt  workspace.FinalizationRequest
	tree     workspace.TreeIdentity
	artifact api.WorkerWorkspaceArtifact
}

type parsedTaskWorkspaceRollback struct {
	receipt workspace.FinalizationRequest
	target  workspace.ResetTarget
	baseID  uuid.UUID
}

func parseTaskCompletionRequest(request api.WorkerCompleteTaskRequest) (parsedTaskCompletion, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedTaskCompletion{}, err
	}
	normalized := request

	parsed := parsedTaskCompletion{lease: lease}
	outcomes := 0
	if request.Outcome.Succeeded != nil {
		outcomes++
		parsed.kind = taskCompletionSucceeded
		if len(request.Outcome.Succeeded.Output) == 0 {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is required")
		}
		if len(request.Outcome.Succeeded.Output) > maxTaskCompletionOutputBytes {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is too large")
		}
		parsed.output, err = canonicalJSON(request.Outcome.Succeeded.Output)
		if err != nil {
			return parsedTaskCompletion{}, fmt.Errorf("outcome.succeeded.output must be an unambiguous JSON value: %w", err)
		}
		if len(parsed.output) > maxTaskCompletionOutputBytes {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is too large")
		}
		normalized.Outcome.Succeeded = &api.WorkerTaskSucceeded{Output: parsed.output}
	}
	if request.Outcome.Failed != nil {
		outcomes++
		parsed.kind = taskCompletionFailed
		parsed.errorObject, normalized.Outcome.Failed, err = normalizeTaskFailure("outcome.failed", request.Outcome.Failed)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
	}
	if request.Outcome.PayloadInvalid != nil {
		outcomes++
		parsed.kind = taskCompletionPayloadInvalid
		parsed.errorObject, normalized.Outcome.PayloadInvalid, err = normalizeTaskFailure("outcome.payload_invalid", request.Outcome.PayloadInvalid)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
	}
	if outcomes != 1 {
		return parsedTaskCompletion{}, errors.New("outcome must contain exactly one variant")
	}

	proofs := 0
	if request.Workspace.Captured != nil {
		proofs++
		capture, normalizedCapture, err := parseTaskWorkspaceCapture(*request.Workspace.Captured)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
		parsed.capture = &capture
		normalized.Workspace.Captured = &normalizedCapture
	}
	if request.Workspace.RolledBack != nil {
		proofs++
		rollback, normalizedRollback, err := parseTaskWorkspaceRollback(*request.Workspace.RolledBack)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
		parsed.rollback = &rollback
		normalized.Workspace.RolledBack = &normalizedRollback
	}
	if proofs != 1 {
		return parsedTaskCompletion{}, errors.New("workspace must contain exactly one proof")
	}
	if parsed.kind == taskCompletionSucceeded && parsed.capture == nil {
		return parsedTaskCompletion{}, errors.New("a successful Task requires a captured Workspace")
	}
	if parsed.kind != taskCompletionSucceeded && parsed.rollback == nil {
		return parsedTaskCompletion{}, errors.New("a failed Task requires a Workspace rollback")
	}
	if request.Handoff != nil {
		if parsed.kind != taskCompletionSucceeded || parsed.capture == nil {
			return parsedTaskCompletion{}, errors.New("a handoff checkpoint requires successful Workspace capture")
		}
		checkpointID, err := parseCanonicalUUID("handoff.checkpoint_id", request.Handoff.CheckpointID)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
		parentRunID, err := parseCanonicalUUID("handoff.manifest.recovery_point.run_id", request.Handoff.Manifest.RecoveryPoint.RunID)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
		waitID, err := parseCanonicalUUID("handoff.manifest.recovery_point.run_wait_id", request.Handoff.Manifest.RecoveryPoint.RunWaitID)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
		if request.Handoff.Manifest.RecoveryPoint.AttemptNumber <= 0 {
			return parsedTaskCompletion{}, errors.New("handoff manifest parent attempt_number must be positive")
		}
		manifest, artifacts, err := validateCheckpointManifest(
			request.Handoff.Manifest,
			checkpointID.String(),
			parentRunID.String(),
			request.Handoff.Manifest.RecoveryPoint.AttemptNumber,
			waitID.String(),
			request.Handoff.Manifest.RecoveryPoint.Runtime.ID,
		)
		if err != nil {
			return parsedTaskCompletion{}, fmt.Errorf("validate handoff checkpoint: %w", err)
		}
		base := request.Handoff.Manifest.WorkspaceState.Base
		if base.ArtifactDigest != parsed.capture.artifact.Digest ||
			base.ArtifactSizeBytes != parsed.capture.artifact.SizeBytes ||
			base.ArtifactMediaType != parsed.capture.artifact.MediaType ||
			base.ArtifactEncoding != parsed.capture.artifact.Encoding ||
			strings.TrimSpace(base.MountPath) == "" {
			return parsedTaskCompletion{}, errors.New("handoff checkpoint Workspace base does not match captured Workspace")
		}
		parsed.handoff = &parsedTaskHandoffCheckpoint{
			checkpointID:  checkpointID,
			parentRunID:   parentRunID,
			waitID:        waitID,
			attemptNumber: request.Handoff.Manifest.RecoveryPoint.AttemptNumber,
			manifest:      manifest,
			artifacts:     artifacts,
		}
	}

	parsed.fingerprint, err = terminalRequestFingerprint("task.complete.v0", normalized)
	if err != nil {
		return parsedTaskCompletion{}, fmt.Errorf("fingerprint Task completion: %w", err)
	}
	return parsed, nil
}

func parseTaskWorkspaceCapture(
	capture api.WorkerTaskWorkspaceCapture,
) (parsedTaskWorkspaceCapture, api.WorkerTaskWorkspaceCapture, error) {
	tree, err := parseTaskWorkspaceTree("workspace.captured.tree", capture.Tree)
	if err != nil {
		return parsedTaskWorkspaceCapture{}, api.WorkerTaskWorkspaceCapture{}, err
	}
	if err := validateTaskWorkspaceArtifact("workspace.captured.artifact", capture.Artifact); err != nil {
		return parsedTaskWorkspaceCapture{}, api.WorkerTaskWorkspaceCapture{}, err
	}
	if int64(capture.Artifact.EntryCount) != int64(tree.EntryCount) {
		return parsedTaskWorkspaceCapture{}, api.WorkerTaskWorkspaceCapture{}, errors.New("workspace.captured artifact and tree entry counts differ")
	}
	receipt, normalizedReceipt, err := parseWorkspaceFinalizationReceipt(
		"workspace.captured.receipt",
		workspace.FinalizationCaptureKind,
		capture.Receipt,
		nil,
	)
	if err != nil {
		return parsedTaskWorkspaceCapture{}, api.WorkerTaskWorkspaceCapture{}, err
	}
	capture.Receipt = normalizedReceipt
	return parsedTaskWorkspaceCapture{receipt: receipt, tree: tree, artifact: capture.Artifact}, capture, nil
}

func parseTaskWorkspaceRollback(
	rollback api.WorkerTaskWorkspaceRollback,
) (parsedTaskWorkspaceRollback, api.WorkerTaskWorkspaceRollback, error) {
	tree, err := parseTaskWorkspaceTree("workspace.rolled_back.target.tree", rollback.Target.Tree)
	if err != nil {
		return parsedTaskWorkspaceRollback{}, api.WorkerTaskWorkspaceRollback{}, err
	}
	baseID, err := parseCanonicalUUID(
		"workspace.rolled_back.target.base_workspace_version_id",
		rollback.Target.BaseWorkspaceVersionID,
	)
	if err != nil {
		return parsedTaskWorkspaceRollback{}, api.WorkerTaskWorkspaceRollback{}, err
	}
	var target workspace.ResetTarget
	switch {
	case rollback.Target.Empty != nil && rollback.Target.Artifact == nil:
		target, err = workspace.EmptyResetTarget(baseID.String(), tree)
	case rollback.Target.Empty == nil && rollback.Target.Artifact != nil:
		if err = validateTaskWorkspaceArtifact("workspace.rolled_back.target.artifact", *rollback.Target.Artifact); err == nil {
			artifact := rollback.Target.Artifact
			if int64(artifact.EntryCount) != int64(tree.EntryCount) {
				err = errors.New("workspace.rolled_back target Artifact and tree entry counts differ")
			} else {
				target, err = workspace.ArtifactResetTarget(baseID.String(), tree, workspace.ArtifactIdentity{
					Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
					SizeBytes: artifact.SizeBytes, EntryCount: int(artifact.EntryCount),
				})
			}
		}
	default:
		err = errors.New("workspace.rolled_back target must contain exactly one source")
	}
	if err != nil {
		return parsedTaskWorkspaceRollback{}, api.WorkerTaskWorkspaceRollback{}, err
	}
	receipt, normalizedReceipt, err := parseWorkspaceFinalizationReceipt(
		"workspace.rolled_back.receipt",
		workspace.FinalizationResetKind,
		rollback.Receipt,
		target,
	)
	if err != nil {
		return parsedTaskWorkspaceRollback{}, api.WorkerTaskWorkspaceRollback{}, err
	}
	rollback.Receipt = normalizedReceipt
	return parsedTaskWorkspaceRollback{receipt: receipt, target: target, baseID: baseID}, rollback, nil
}

func parseWorkspaceFinalizationReceipt(
	label string,
	kind string,
	receipt api.WorkerWorkspaceFinalizationReceipt,
	target any,
) (workspace.FinalizationRequest, api.WorkerWorkspaceFinalizationReceipt, error) {
	operationID, err := parseCanonicalUUID(label+".operation_id", receipt.OperationID)
	if err != nil {
		return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, err
	}
	if !taskWorkspaceDigestPattern.MatchString(receipt.RequestFingerprint) {
		return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, fmt.Errorf("%s.request_fingerprint must be a SHA-256 digest", label)
	}
	if receipt.Fence.AttemptNumber <= 0 || receipt.Fence.ExpiresAt.IsZero() {
		return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, fmt.Errorf("%s fence is invalid", label)
	}
	expiresAtUnixNano := receipt.Fence.ExpiresAt.UnixNano()
	if !time.Unix(0, expiresAtUnixNano).Equal(receipt.Fence.ExpiresAt) {
		return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, fmt.Errorf("%s.fence.expires_at is outside the finalization protocol range", label)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "worker_instance_id", value: receipt.Fence.WorkerInstanceID},
		{name: "runtime_instance_id", value: receipt.Fence.RuntimeInstanceID},
		{name: "workspace_id", value: receipt.Fence.WorkspaceID},
		{name: "workspace_mount_id", value: receipt.Fence.WorkspaceMountID},
		{name: "run_id", value: receipt.Fence.RunID},
		{name: "run_lease_id", value: receipt.Fence.RunLeaseID},
		{name: "workspace_lease_id", value: receipt.Fence.WorkspaceLeaseID},
		{name: "base_workspace_version_id", value: receipt.Fence.BaseWorkspaceVersionID},
	} {
		if _, err := parseCanonicalUUID(label+".fence."+field.name, field.value); err != nil {
			return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, err
		}
	}
	if strings.TrimSpace(receipt.Fence.RuntimeIdentityID) == "" ||
		strings.TrimSpace(receipt.Fence.RuntimeIdentityID) != receipt.Fence.RuntimeIdentityID {
		return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, fmt.Errorf("%s.fence.runtime_identity_id is invalid", label)
	}
	if receipt.Fence.WorkerEpoch <= 0 || receipt.Fence.LeaseSequence <= 0 ||
		receipt.Fence.OwnershipGeneration < 0 || receipt.Fence.WriterGeneration <= 0 ||
		receipt.Fence.MountFencingGeneration <= 0 {
		return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, fmt.Errorf("%s fence generations are invalid", label)
	}
	fence := workspace.FinalizationFence{
		WorkerInstanceID: receipt.Fence.WorkerInstanceID, WorkerEpoch: receipt.Fence.WorkerEpoch,
		RuntimeInstanceID: receipt.Fence.RuntimeInstanceID, RuntimeIdentityID: receipt.Fence.RuntimeIdentityID,
		WorkspaceID: receipt.Fence.WorkspaceID, WorkspaceMountID: receipt.Fence.WorkspaceMountID,
		RunID: receipt.Fence.RunID, AttemptNumber: uint32(receipt.Fence.AttemptNumber),
		RunLeaseID: receipt.Fence.RunLeaseID, LeaseSequence: receipt.Fence.LeaseSequence,
		WorkspaceLeaseID:    receipt.Fence.WorkspaceLeaseID,
		OwnershipGeneration: receipt.Fence.OwnershipGeneration, WriterGeneration: receipt.Fence.WriterGeneration,
		MountFencingGeneration: receipt.Fence.MountFencingGeneration,
		ExpiresAtUnixNano:      expiresAtUnixNano, BaseWorkspaceVersionID: receipt.Fence.BaseWorkspaceVersionID,
	}
	request := workspace.FinalizationRequest{OperationID: operationID.String(), Fence: fence, Target: target}
	expected, err := workspace.FinalizationFingerprint(kind, request)
	if err != nil || expected != receipt.RequestFingerprint {
		return workspace.FinalizationRequest{}, api.WorkerWorkspaceFinalizationReceipt{}, fmt.Errorf("%s request fingerprint is invalid", label)
	}
	receipt.Fence.ExpiresAt = receipt.Fence.ExpiresAt.UTC()
	return request, receipt, nil
}

func finalizationFenceMatchesLease(fence workspace.FinalizationFence, lease api.WorkerRunLeaseAssignment) bool {
	return fence.WorkerInstanceID == lease.WorkerInstanceID &&
		fence.WorkerEpoch == lease.WorkerEpoch &&
		fence.RuntimeInstanceID == lease.RuntimeInstanceID &&
		fence.RuntimeIdentityID == lease.RuntimeIdentityID &&
		fence.WorkspaceID == lease.WorkspaceID &&
		fence.WorkspaceMountID == lease.WorkspaceMountID &&
		fence.RunID == lease.RunID &&
		fence.AttemptNumber == uint32(lease.AttemptNumber) &&
		fence.RunLeaseID == lease.ID &&
		fence.LeaseSequence == lease.LeaseSequence &&
		fence.WorkspaceLeaseID == lease.WorkspaceLeaseID &&
		fence.OwnershipGeneration == lease.OwnershipGeneration &&
		fence.WriterGeneration == lease.WriterGeneration &&
		fence.MountFencingGeneration == lease.MountFencingGeneration &&
		fence.ExpiresAtUnixNano == lease.ExpiresAt.UnixNano() &&
		fence.BaseWorkspaceVersionID == lease.BaseWorkspaceVersionID
}

func parseTaskWorkspaceTree(label string, tree api.WorkerWorkspaceTreeIdentity) (workspace.TreeIdentity, error) {
	if !taskWorkspaceDigestPattern.MatchString(tree.Digest) || tree.SizeBytes < 0 ||
		tree.SizeBytes > workspace.MaxArtifactExtractedBytes || tree.EntryCount < 0 ||
		int64(tree.EntryCount) > int64(workspace.MaxArtifactEntries) {
		return workspace.TreeIdentity{}, fmt.Errorf("%s is invalid", label)
	}
	return workspace.TreeIdentity{Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: int(tree.EntryCount)}, nil
}

func normalizeTaskFailure(label string, failure *api.WorkerTaskFailure) (json.RawMessage, *api.WorkerTaskFailure, error) {
	if !utf8.ValidString(failure.Message) || len(failure.Message) > maxTaskCompletionMessageBytes {
		return nil, nil, fmt.Errorf("%s.message must be valid UTF-8 no larger than %d bytes", label, maxTaskCompletionMessageBytes)
	}
	var details json.RawMessage
	if len(failure.Details) != 0 {
		if len(failure.Details) > maxTaskCompletionErrorBytes {
			return nil, nil, fmt.Errorf("%s.details is too large", label)
		}
		canonical, err := canonicalJSON(failure.Details)
		if err != nil {
			return nil, nil, fmt.Errorf("%s.details must be an unambiguous JSON value: %w", label, err)
		}
		details = canonical
	}
	errorObject, err := json.Marshal(struct {
		Message string          `json:"message"`
		Details json.RawMessage `json:"details,omitempty"`
	}{Message: failure.Message, Details: details})
	if err != nil {
		return nil, nil, fmt.Errorf("encode %s: %w", label, err)
	}
	errorObject, err = canonicalJSON(errorObject)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if len(errorObject) > maxTaskCompletionErrorBytes {
		return nil, nil, fmt.Errorf("%s exceeds %d bytes", label, maxTaskCompletionErrorBytes)
	}
	return errorObject, &api.WorkerTaskFailure{Message: failure.Message, Details: details}, nil
}

func validateTaskWorkspaceArtifact(label string, artifact api.WorkerWorkspaceArtifact) error {
	if !taskWorkspaceDigestPattern.MatchString(artifact.Digest) {
		return fmt.Errorf("%s.digest must be a SHA-256 digest", label)
	}
	if artifact.MediaType != workspace.ArtifactMediaType {
		return fmt.Errorf("%s.media_type is unsupported", label)
	}
	if artifact.Encoding != workspace.ArtifactEncoding {
		return fmt.Errorf("%s.encoding is unsupported", label)
	}
	if artifact.SizeBytes <= 0 || artifact.SizeBytes > workspace.MaxArtifactArchiveBytes {
		return fmt.Errorf("%s.size_bytes must be between 1 and %d", label, workspace.MaxArtifactArchiveBytes)
	}
	if artifact.EntryCount < 0 || artifact.EntryCount > workspace.MaxArtifactEntries {
		return fmt.Errorf("%s.entry_count must be between 0 and %d", label, workspace.MaxArtifactEntries)
	}
	return nil
}

func parseCanonicalUUID(name, value string) (uuid.UUID, error) {
	parsed, err := ids.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s must be a canonical UUIDv7", name)
	}
	return parsed, nil
}
