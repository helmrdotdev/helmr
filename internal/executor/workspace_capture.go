package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

type WorkspaceCapture struct {
	Receipt      *workspacev0.WorkspaceFinalizationReceipt
	ReportedTree workspace.TreeIdentity
	Artifact     workspace.WorkspaceArtifact
}

func captureWorkspaceOnSession(ctx context.Context, session vm.Session, store cas.Store, request *workspacev0.CaptureWorkspaceRequest) (WorkspaceCapture, error) {
	if session == nil || store == nil {
		return WorkspaceCapture{}, errors.New("Workspace Capture session and CAS are required")
	}
	if request == nil || request.GetEnvelope() == nil || request.GetEnvelope().GetAuthority() == nil || request.GetEnvelope().GetAuthority().GetFence() == nil {
		return WorkspaceCapture{}, errors.New("Workspace Capture envelope is required")
	}
	envelope := request.GetEnvelope()
	fence := envelope.GetAuthority().GetFence()
	expectedFingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationCaptureKind, workspace.FinalizationRequest{
		OperationID: envelope.GetOperationId(),
		Fence:       executorFinalizationFence(fence),
	})
	if err != nil || expectedFingerprint != envelope.GetRequestFingerprint() {
		return WorkspaceCapture{}, errors.New("Workspace Capture request fingerprint is invalid")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return WorkspaceCapture{}, fmt.Errorf("open Workspace Capture stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceCapture,
		RunID:            fence.GetRunId(),
		WorkspaceID:      fence.GetWorkspaceId(),
		WorkspaceMountID: fence.GetWorkspaceMountId(),
		OperationID:      envelope.GetOperationId(),
	}, 0); err != nil {
		return WorkspaceCapture{}, fmt.Errorf("write Workspace Capture header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return WorkspaceCapture{}, fmt.Errorf("write Workspace Capture request: %w", err)
	}
	var response workspacev0.CaptureWorkspaceResponse
	if err := readWorkspaceControlResponse(ctx, stream, &response); err != nil {
		return WorkspaceCapture{}, fmt.Errorf("read Workspace Capture response: %w", err)
	}
	if strings.TrimSpace(response.GetError()) != "" {
		return WorkspaceCapture{}, fmt.Errorf("Workspace Capture failed: %s", response.GetError())
	}
	if response.GetReceipt() == nil ||
		response.GetReceipt().GetOperationId() != envelope.GetOperationId() ||
		response.GetReceipt().GetRequestFingerprint() != envelope.GetRequestFingerprint() ||
		!proto.Equal(response.GetReceipt().GetFence(), fence) {
		return WorkspaceCapture{}, errors.New("Workspace Capture receipt does not match the request")
	}
	tree := response.GetTree()
	if tree == nil || !validSHA256Digest(tree.GetDigest()) || tree.GetSizeBytes() < 0 || tree.GetSizeBytes() > workspace.MaxArtifactExtractedBytes || tree.GetEntryCount() > uint32(workspace.MaxArtifactEntries) {
		return WorkspaceCapture{}, errors.New("Workspace Capture tree identity is invalid")
	}
	artifact := response.GetArtifact()
	if artifact == nil || !validSHA256Digest(artifact.GetDigest()) ||
		artifact.GetMediaType() != workspace.ArtifactMediaType ||
		artifact.GetEncoding() != workspace.ArtifactEncoding ||
		artifact.GetSizeBytes() == 0 || artifact.GetSizeBytes() > uint64(workspace.MaxArtifactArchiveBytes) ||
		artifact.GetEntryCount() > uint32(workspace.MaxArtifactEntries) {
		return WorkspaceCapture{}, errors.New("Workspace Capture Artifact descriptor is invalid")
	}
	header, bodyLength, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		return WorkspaceCapture{}, fmt.Errorf("read Workspace Capture Artifact header: %w", err)
	}
	if header.Type != wire.StreamTypeWorkspaceArtifact ||
		header.WorkspaceID != fence.GetWorkspaceId() ||
		header.OperationID != envelope.GetOperationId() ||
		header.BodyDigest == nil || strings.TrimSpace(*header.BodyDigest) != artifact.GetDigest() ||
		bodyLength != artifact.GetSizeBytes() ||
		header.EntryCount == nil || *header.EntryCount != int(artifact.GetEntryCount()) {
		return WorkspaceCapture{}, errors.New("Workspace Capture Artifact frame does not match its receipt")
	}
	body := &io.LimitedReader{R: stream, N: int64(bodyLength)}
	object, err := store.Put(ctx, workspace.ArtifactMediaType, body)
	if err != nil {
		return WorkspaceCapture{}, fmt.Errorf("store Workspace Capture Artifact: %w", err)
	}
	if body.N != 0 {
		return WorkspaceCapture{}, errors.New("Workspace Capture Artifact stream ended early")
	}
	if object.Digest != artifact.GetDigest() || object.SizeBytes != int64(artifact.GetSizeBytes()) || object.MediaType != workspace.ArtifactMediaType {
		return WorkspaceCapture{}, errors.New("Workspace Capture CAS object does not match its receipt")
	}
	return WorkspaceCapture{
		Receipt: response.GetReceipt(),
		ReportedTree: workspace.TreeIdentity{
			Digest:     tree.GetDigest(),
			SizeBytes:  tree.GetSizeBytes(),
			EntryCount: int(tree.GetEntryCount()),
		},
		Artifact: workspace.WorkspaceArtifact{
			Digest:     object.Digest,
			MediaType:  object.MediaType,
			Encoding:   artifact.GetEncoding(),
			SizeBytes:  object.SizeBytes,
			EntryCount: int(artifact.GetEntryCount()),
		},
	}, nil
}

func readWorkspaceControlResponse(ctx context.Context, stream vm.Stream, message proto.Message) error {
	result := make(chan error, 1)
	go func() { result <- frameio.ReadProtoFrame(stream, message) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = stream.Close()
		return ctx.Err()
	}
}

func executorFinalizationFence(fence *workspacev0.WorkspaceAuthorityFence) workspace.FinalizationFence {
	if fence == nil {
		return workspace.FinalizationFence{}
	}
	return workspace.FinalizationFence{
		WorkerInstanceID:       fence.GetWorkerInstanceId(),
		WorkerEpoch:            fence.GetWorkerEpoch(),
		RuntimeInstanceID:      fence.GetRuntimeInstanceId(),
		RuntimeIdentityID:      fence.GetRuntimeIdentityId(),
		WorkspaceID:            fence.GetWorkspaceId(),
		WorkspaceMountID:       fence.GetWorkspaceMountId(),
		RunID:                  fence.GetRunId(),
		AttemptNumber:          fence.GetAttemptNumber(),
		RunLeaseID:             fence.GetRunLeaseId(),
		LeaseSequence:          fence.GetLeaseSequence(),
		WorkspaceLeaseID:       fence.GetWorkspaceLeaseId(),
		OwnershipGeneration:    fence.GetOwnershipGeneration(),
		WriterGeneration:       fence.GetWriterGeneration(),
		MountFencingGeneration: fence.GetMountFencingGeneration(),
		ExpiresAtUnixNano:      fence.GetExpiresAtUnixNano(),
		BaseWorkspaceVersionID: fence.GetBaseWorkspaceVersionId(),
	}
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
