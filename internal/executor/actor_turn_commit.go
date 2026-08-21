package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
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
	if task.finished || task.finalizingKind != "" {
		task.mu.Unlock()
		return errors.New("run lease task cannot commit an actor turn")
	}
	if task.store == nil {
		task.mu.Unlock()
		return errors.New("actor turn commit CAS is required")
	}
	stream := task.program.session.Stream()
	lease := task.lease
	currentTarget := task.resetTarget
	expected := currentTarget.Tree
	expectedBase := currentTarget.BaseVersionID
	task.mu.Unlock()
	stopClose := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stopClose()
	pause := &runv0.ActorTurnCommitPauseRequest{
		CorrelationId: requested.GetCorrelationId(), TargetInputSequence: requested.GetTargetInputSequence(),
		RunId: lease.RunID, AttemptNumber: uint32(lease.AttemptNumber), RunLeaseId: lease.ID,
		ExpectedTreeDigest: expected.Digest, ExpectedTreeSizeBytes: expected.SizeBytes,
		ExpectedTreeEntryCount:         uint32(expected.EntryCount),
		ExpectedBaseWorkspaceVersionId: expectedBase,
	}
	if err := wire.WriteActorTurnCommitPauseRequest(stream, pause); err != nil {
		return fmt.Errorf("write actor turn commit pause request: %w", err)
	}
	reader := bufio.NewReader(stream)
	ready, artifact, err := task.readActorTurnCommitReady(ctx, reader, pause)
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
		CorrelationID:          requested.GetCorrelationId(),
		TargetInputSequence:    requested.GetTargetInputSequence(),
		BaseWorkspaceVersionID: expectedBase,
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
	task.renewalGate.Lock()
	defer task.renewalGate.Unlock()
	pendingBase, err := task.commitActorTurnWithRenewal(ctx, expectedBase, &request, &response)
	if err != nil {
		return fmt.Errorf("commit actor turn: %w", err)
	}
	if response.Lease != request.Lease ||
		response.CorrelationID != requested.GetCorrelationId() ||
		response.CommittedInputSequence != requested.GetTargetInputSequence() ||
		strings.TrimSpace(response.WorkspaceVersionID) == "" ||
		response.Tree != request.Tree {
		return errors.New("actor turn commit response did not match the request")
	}
	if pendingBase != "" && response.WorkspaceVersionID != pendingBase {
		return errors.New("actor turn commit response did not match the renewed pending frontier")
	}
	nextTarget := currentTarget
	if artifact == nil {
		if response.WorkspaceVersionID != expectedBase {
			return errors.New("unchanged actor turn commit replaced the workspace version")
		}
		nextTarget.BaseVersionID = response.WorkspaceVersionID
	} else {
		nextTarget, err = workspace.ArtifactResetTarget(
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
	}
	checkpointBase, err := checkpointWorkspaceBase(nextTarget)
	if err != nil {
		return fmt.Errorf("advance checkpoint workspace base: %w", err)
	}
	decisionData, err := json.Marshal(struct {
		WorkspaceVersionID string `json:"workspace_version_id"`
	}{WorkspaceVersionID: response.WorkspaceVersionID})
	if err != nil {
		return fmt.Errorf("encode actor turn commit decision: %w", err)
	}
	task.mu.Lock()
	proofDeadline := task.lease.ExpiresAt
	task.mu.Unlock()
	proofCtx, cancelProof := context.WithDeadline(ctx, proofDeadline)
	defer cancelProof()
	stopProofClose := context.AfterFunc(proofCtx, func() { _ = stream.Close() })
	defer stopProofClose()
	if err := wire.WriteResumeDecision(stream, &runv0.ResumeDecision{
		CorrelationId: requested.GetCorrelationId(), Kind: "committed", DataJson: string(decisionData),
	}); err != nil {
		return fmt.Errorf("write actor turn commit decision: %w", err)
	}
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		return fmt.Errorf("read actor turn commit applied header: %w", err)
	}
	applied, err := wire.ReadActorTurnCommitApplied(header, reader, bodyLen)
	if err != nil {
		return err
	}
	if applied.GetRunId() != pause.GetRunId() || applied.GetAttemptNumber() != pause.GetAttemptNumber() ||
		applied.GetRunLeaseId() != pause.GetRunLeaseId() || applied.GetCorrelationId() != pause.GetCorrelationId() ||
		applied.GetTargetInputSequence() != pause.GetTargetInputSequence() ||
		applied.GetPreviousBaseWorkspaceVersionId() != expectedBase ||
		applied.GetAppliedBaseWorkspaceVersionId() != response.WorkspaceVersionID {
		return errors.New("actor turn commit applied proof did not match the exact frontier transition")
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if err := proofCtx.Err(); err != nil {
		return fmt.Errorf("actor turn commit applied proof exceeded lease authority: %w", err)
	}
	if task.finished || task.finalizingKind != "" || task.authority == nil || task.authority.GetFence() == nil ||
		task.resetTarget.BaseVersionID != expectedBase || task.lease.BaseWorkspaceVersionID != expectedBase ||
		task.authority.GetFence().GetBaseWorkspaceVersionId() != expectedBase {
		return errors.New("actor turn commit local Workspace frontier changed during transition")
	}
	task.resetTarget = nextTarget
	if checkpointer, ok := task.checkpointer.(*runtimeCheckpointer); ok {
		checkpointer.workspace = checkpointBase
	}
	task.waitWorkspace.BaseVersionID = response.WorkspaceVersionID
	if artifact != nil {
		task.waitWorkspace.Artifact = &workerapi.WorkspaceArtifact{
			Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
			SizeBytes: artifact.SizeBytes, EntryCount: int32(artifact.EntryCount),
		}
	}
	task.lease.BaseWorkspaceVersionID = response.WorkspaceVersionID
	task.authority.Fence.BaseWorkspaceVersionId = response.WorkspaceVersionID
	return nil
}

func (task *guestRunLeaseTask) commitActorTurnWithRenewal(
	ctx context.Context,
	expectedBase string,
	request *workerapi.CommitActorTurnRequest,
	response *workerapi.CommitActorTurnResponse,
) (string, error) {
	done := make(chan error, 1)
	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	go func() {
		done <- task.callRunSourceRuntime(operationCtx, func(
			requestCtx context.Context,
			current workerapi.RunLeaseAssignment,
		) error {
			request.Lease = current.Fence()
			var requestErr error
			*response, requestErr = task.controlPlane.CommitActorTurn(requestCtx, *request)
			return requestErr
		})
	}()
	task.mu.Lock()
	expiresAt := task.lease.ExpiresAt
	task.mu.Unlock()
	renewTimer := time.NewTimer(runLeaseRenewDelay(expiresAt))
	defer renewTimer.Stop()
	pendingBase := ""
	for {
		select {
		case err := <-done:
			return pendingBase, err
		case <-renewTimer.C:
			var err error
			pendingBase, expiresAt, err = task.renewActorTurnPendingAuthority(
				ctx, expectedBase, pendingBase,
			)
			if err != nil {
				cancelOperation()
				<-done
				return pendingBase, err
			}
			renewTimer.Reset(runLeaseRenewDelay(expiresAt))
		case <-ctx.Done():
			cancelOperation()
			<-done
			return pendingBase, ctx.Err()
		}
	}
}

func (task *guestRunLeaseTask) renewActorTurnPendingAuthority(
	ctx context.Context,
	expectedBase string,
	pendingBase string,
) (string, time.Time, error) {
	task.mu.Lock()
	if task.finished || task.finalizingKind != "" || task.lease.BaseWorkspaceVersionID != expectedBase ||
		task.authority == nil || task.authority.GetFence() == nil ||
		task.authority.GetFence().GetBaseWorkspaceVersionId() != expectedBase {
		task.mu.Unlock()
		return pendingBase, time.Time{}, errors.New("actor turn pending authority is no longer current")
	}
	previous := task.lease
	authority := proto.Clone(task.authority).(*workspacev0.WorkspaceRunAuthority)
	task.mu.Unlock()
	renewed, projectedBase, err := renewControlPlaneActorTurnAuthority(ctx, task.controlPlane, previous)
	if err != nil {
		return pendingBase, time.Time{}, err
	}
	switch {
	case projectedBase == expectedBase:
	case pendingBase == "":
		pendingBase = projectedBase
	case projectedBase != pendingBase:
		return pendingBase, time.Time{}, errors.New("actor turn pending Workspace frontier changed during renewal")
	}
	var fence *workspacev0.WorkspaceAuthorityFence
	if renewed.ExpiresAt.After(previous.ExpiresAt) {
		guestCtx, cancelGuest := context.WithDeadline(context.Background(), renewed.ExpiresAt)
		defer cancelGuest()
		if err := retryWorkspaceAuthorityTransport(guestCtx, func(requestCtx context.Context) error {
			var requestErr error
			fence, requestErr = task.mounts.RenewWorkspaceAuthority(
				requestCtx,
				&workspacev0.RenewWorkspaceAuthorityRequest{
					Previous: authority, NewExpiresAtUnixNano: renewed.ExpiresAt.UnixNano(),
				},
			)
			return requestErr
		}); err != nil {
			return pendingBase, time.Time{}, err
		}
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.lease.BaseWorkspaceVersionID != expectedBase || !task.lease.ExpiresAt.Equal(previous.ExpiresAt) ||
		task.authority == nil || task.authority.GetFence() == nil ||
		task.authority.GetFence().GetBaseWorkspaceVersionId() != expectedBase {
		return pendingBase, time.Time{}, errors.New("actor turn pending authority changed during renewal")
	}
	if fence != nil {
		task.authority.Fence = proto.Clone(fence).(*workspacev0.WorkspaceAuthorityFence)
	}
	task.lease.ExpiresAt = renewed.ExpiresAt
	return pendingBase, renewed.ExpiresAt, nil
}

func renewControlPlaneActorTurnAuthority(
	ctx context.Context,
	controlPlane interface {
		RenewRunLease(context.Context, workerapi.RunLeaseAssignment) (workerapi.RunLeaseRenewResponse, error)
	},
	previous workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseAssignment, string, error) {
	controlCtx, cancelControlPlane := context.WithDeadline(ctx, previous.ExpiresAt)
	defer cancelControlPlane()
	var response workerapi.RunLeaseRenewResponse
	if err := retryRunLeaseRequest(controlCtx, func(requestCtx context.Context) error {
		var requestErr error
		response, requestErr = controlPlane.RenewRunLease(requestCtx, previous)
		return requestErr
	}); err != nil {
		if !previous.ExpiresAt.After(time.Now()) {
			return workerapi.RunLeaseAssignment{}, "", fmt.Errorf("%w: %v", errRunLeaseAuthorityLapsed, err)
		}
		return workerapi.RunLeaseAssignment{}, "", err
	}
	projectedBase := strings.TrimSpace(response.BaseWorkspaceVersionID)
	if response.Lease != previous.Fence() || projectedBase == "" {
		return workerapi.RunLeaseAssignment{}, "", errors.New("actor turn renewal response changed its fence or omitted its Workspace frontier")
	}
	renewed := previous
	renewed.ExpiresAt = response.ExpiresAt
	if err := validateRunLeaseExpiryAdvance(previous, renewed); err != nil {
		return workerapi.RunLeaseAssignment{}, "", err
	}
	return renewed, projectedBase, nil
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
			err = taskControlEvents{task: task}.AppendRunLog(ctx, workerapi.RunLeaseAssignment{}, workerapi.LogStreamStdout, task.program.observedEventSeq, value.StdoutChunk)
		case *runv0.RunEvent_StderrChunk:
			err = taskControlEvents{task: task}.AppendRunLog(ctx, workerapi.RunLeaseAssignment{}, workerapi.LogStreamStderr, task.program.observedEventSeq, value.StderrChunk)
		case *runv0.RunEvent_MetadataUpdated:
			err = taskControlEvents{task: task}.ApplyRunMetadata(ctx, workerapi.RunLeaseAssignment{}, value.MetadataUpdated)
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
			err = taskControlEvents{task: task}.RecordStructuredRunLog(
				ctx, workerapi.RunLeaseAssignment{}, task.program.observedEventSeq, value.StructuredLogRequested,
			)
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
