package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func (task *guestRunLeaseTask) handleActorTurnCommit(
	ctx context.Context,
	requested *runv0.ActorTurnCommitRequested,
) error {
	if requested == nil || strings.TrimSpace(requested.GetCorrelationId()) == "" || requested.GetTargetInputSequence() <= 0 {
		return errors.New("actor turn commit request is invalid")
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != "" {
		return errors.New("run lease task cannot commit an actor turn")
	}
	if task.store == nil {
		return errors.New("actor turn commit CAS is required")
	}
	stream := task.program.session.Stream()
	turnCtx, cancelTurn := context.WithDeadline(ctx, task.lease.ExpiresAt)
	stopClose := context.AfterFunc(turnCtx, func() { _ = stream.Close() })
	defer func() {
		stopClose()
		cancelTurn()
	}()
	expected := task.resetTarget.Tree
	pause := &runv0.ActorTurnCommitPauseRequest{
		CorrelationId: requested.GetCorrelationId(), TargetInputSequence: requested.GetTargetInputSequence(),
		RunId: task.lease.RunID, AttemptNumber: uint32(task.lease.AttemptNumber), RunLeaseId: task.lease.ID,
		ExpectedTreeDigest: expected.Digest, ExpectedTreeSizeBytes: expected.SizeBytes,
		ExpectedTreeEntryCount:         uint32(expected.EntryCount),
		ExpectedBaseWorkspaceVersionId: task.resetTarget.BaseVersionID,
	}
	if err := wire.WriteActorTurnCommitPauseRequest(stream, pause); err != nil {
		return fmt.Errorf("write actor turn commit pause request: %w", err)
	}
	ready, artifact, err := task.readActorTurnCommitReady(turnCtx, bufio.NewReader(stream), pause)
	if err != nil {
		return err
	}
	tree := workspace.TreeIdentity{
		Digest: ready.GetTreeDigest(), SizeBytes: ready.GetTreeSizeBytes(),
		EntryCount: int(ready.GetTreeEntryCount()),
	}
	if err := workspace.ValidateTreeIdentity(tree); err != nil {
		return fmt.Errorf("validate actor turn commit tree: %w", err)
	}
	if ready.GetWorkspaceChanged() != (tree != expected) || ready.GetWorkspaceChanged() != (artifact != nil) {
		return errors.New("actor turn commit capture did not match its tree proof")
	}
	request := workerapi.CommitActorTurnRequest{
		Lease: task.lease.Fence(), CorrelationID: requested.GetCorrelationId(),
		TargetInputSequence:    requested.GetTargetInputSequence(),
		BaseWorkspaceVersionID: task.resetTarget.BaseVersionID,
		Tree: workerapi.WorkspaceTreeIdentity{
			Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: int32(tree.EntryCount),
		},
	}
	if artifact != nil {
		request.Artifact = &workerapi.WorkspaceArtifact{
			Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
			SizeBytes: artifact.SizeBytes, EntryCount: int32(artifact.EntryCount),
		}
	}
	var response workerapi.CommitActorTurnResponse
	if err := retryRunLeaseRequest(turnCtx, func(requestCtx context.Context) error {
		var requestErr error
		response, requestErr = task.controlPlane.CommitActorTurn(requestCtx, request)
		return requestErr
	}); err != nil {
		return fmt.Errorf("commit actor turn: %w", err)
	}
	if response.Lease != task.lease.Fence() ||
		response.CorrelationID != requested.GetCorrelationId() ||
		response.CommittedInputSequence != requested.GetTargetInputSequence() ||
		strings.TrimSpace(response.WorkspaceVersionID) == "" ||
		response.Tree != request.Tree {
		return errors.New("actor turn commit response did not match the request")
	}
	if task.authority == nil || task.authority.GetFence() == nil ||
		task.authority.GetFence().GetBaseWorkspaceVersionId() != request.BaseWorkspaceVersionID {
		return errors.New("actor turn commit workspace authority frontier is stale")
	}
	if artifact == nil {
		if response.WorkspaceVersionID != task.resetTarget.BaseVersionID {
			return errors.New("unchanged actor turn commit replaced the workspace version")
		}
		task.resetTarget.BaseVersionID = response.WorkspaceVersionID
	} else {
		task.resetTarget, err = workspace.ArtifactResetTarget(
			response.WorkspaceVersionID,
			tree,
			workspace.ArtifactIdentity{
				Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
				SizeBytes: artifact.SizeBytes, EntryCount: artifact.EntryCount,
			},
		)
		if err != nil {
			return fmt.Errorf("advance actor turn reset target: %w", err)
		}
		committedArtifact := &workerapi.WorkspaceArtifact{
			Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
			SizeBytes: artifact.SizeBytes, EntryCount: int32(artifact.EntryCount),
		}
		task.waitWorkspace.Artifact = committedArtifact
	}
	checkpointBase, err := checkpointWorkspaceBase(task.resetTarget)
	if err != nil {
		return fmt.Errorf("advance checkpoint workspace base: %w", err)
	}
	if checkpointer, ok := task.checkpointer.(*runtimeCheckpointer); ok {
		checkpointer.workspace = checkpointBase
	}
	task.waitWorkspace.BaseVersionID = response.WorkspaceVersionID
	task.lease.BaseWorkspaceVersionID = response.WorkspaceVersionID
	task.authority.Fence.BaseWorkspaceVersionId = response.WorkspaceVersionID
	decisionData, err := json.Marshal(struct {
		WorkspaceVersionID string `json:"workspace_version_id"`
	}{WorkspaceVersionID: response.WorkspaceVersionID})
	if err != nil {
		return fmt.Errorf("encode actor turn commit decision: %w", err)
	}
	if err := wire.WriteResumeDecision(stream, &runv0.ResumeDecision{
		CorrelationId: requested.GetCorrelationId(), Kind: "committed", DataJson: string(decisionData),
	}); err != nil {
		return fmt.Errorf("write actor turn commit decision: %w", err)
	}
	return nil
}

func (task *guestRunLeaseTask) readActorTurnCommitReady(
	ctx context.Context,
	reader *bufio.Reader,
	request *runv0.ActorTurnCommitPauseRequest,
) (*runv0.ActorTurnCommitPauseReady, *workspace.WorkspaceArtifact, error) {
	var artifact *workspace.WorkspaceArtifact
	for {
		prefix, err := reader.Peek(4)
		if err != nil {
			return nil, nil, err
		}
		if frameio.IsStreamFramePrefix(prefix) {
			header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
			if err != nil {
				return nil, nil, err
			}
			switch header.Type {
			case wire.StreamTypeWorkspaceArtifact:
				if artifact != nil {
					return nil, nil, errors.New("actor turn commit returned multiple workspace captures")
				}
				captured, err := storeWorkspaceArtifactFrame(ctx, task.store, reader, header, bodyLen, request.GetRunId())
				if err != nil {
					return nil, nil, err
				}
				artifact = &captured
			case wire.StreamTypeActorTurnCommitReady:
				ready, err := wire.ReadActorTurnCommitPauseReady(header, reader, bodyLen)
				if err != nil {
					return nil, nil, err
				}
				if ready.GetCorrelationId() != request.GetCorrelationId() ||
					ready.GetTargetInputSequence() != request.GetTargetInputSequence() ||
					ready.GetRunId() != request.GetRunId() || ready.GetAttemptNumber() != request.GetAttemptNumber() ||
					ready.GetRunLeaseId() != request.GetRunLeaseId() {
					return nil, nil, errors.New("actor turn commit pause proof did not match its request")
				}
				return ready, artifact, nil
			default:
				return nil, nil, fmt.Errorf("unsupported actor turn commit stream type %q", header.Type)
			}
			continue
		}
		body, err := frameio.ReadMessageFrame(reader)
		if err != nil {
			return nil, nil, err
		}
		var event runv0.RunEvent
		if err := proto.Unmarshal(body, &event); err != nil {
			return nil, nil, fmt.Errorf("unmarshal actor turn commit interleaved event: %w", err)
		}
		task.program.observedEventSeq++
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			err = task.controlPlane.AppendRunLog(ctx, task.lease, workerapi.LogStreamStdout, task.program.observedEventSeq, value.StdoutChunk)
		case *runv0.RunEvent_StderrChunk:
			err = task.controlPlane.AppendRunLog(ctx, task.lease, workerapi.LogStreamStderr, task.program.observedEventSeq, value.StderrChunk)
		case *runv0.RunEvent_MetadataUpdated:
			var observability runObservabilityControlPlane
			observability, err = requireRunObservabilityControlPlane(task.controlPlane)
			if err == nil {
				err = updateRunMetadata(ctx, observability, task.lease, value.MetadataUpdated)
			}
			if err == nil || isRuntimeOperationRejection(err) {
				err = writeRuntimeOperationDecision(
					task.program.session.Stream(),
					value.MetadataUpdated.GetCorrelationId(),
					err,
					"run_metadata_rejected",
					"Run metadata request was rejected",
				)
			}
		case *runv0.RunEvent_StructuredLogRequested:
			var observability runObservabilityControlPlane
			observability, err = requireRunObservabilityControlPlane(task.controlPlane)
			if err == nil {
				err = appendStructuredRunLog(
					ctx,
					observability,
					task.lease,
					task.program.observedEventSeq,
					value.StructuredLogRequested,
				)
			}
			if err == nil || isRuntimeOperationRejection(err) {
				err = writeRuntimeOperationDecision(
					task.program.session.Stream(),
					value.StructuredLogRequested.GetCorrelationId(),
					err,
					"structured_log_rejected",
					"Structured log request was rejected",
				)
			}
		default:
			err = errors.New("unsupported program event while actor turn commit is pending")
		}
		if err != nil {
			return nil, nil, err
		}
	}
}
