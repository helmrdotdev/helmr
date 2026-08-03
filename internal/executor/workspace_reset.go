package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type WorkspaceReset struct {
	Receipt *workspacev0.WorkspaceFinalizationReceipt
	Target  workspace.ResetTarget
}

func resetWorkspaceOnSession(ctx context.Context, session vm.Session, store cas.Reader, request *workspacev0.ResetWorkspaceRequest) (WorkspaceReset, error) {
	if session == nil {
		return WorkspaceReset{}, errors.New("workspace reset session is required")
	}
	if request == nil || request.GetEnvelope() == nil || request.GetEnvelope().GetAuthority() == nil || request.GetEnvelope().GetAuthority().GetFence() == nil {
		return WorkspaceReset{}, errors.New("workspace reset envelope is required")
	}
	target, err := workspace.ResetTargetFromProto(request.GetTarget())
	if err != nil {
		return WorkspaceReset{}, err
	}
	if target.Kind == workspace.ResetTargetArtifact && store == nil {
		return WorkspaceReset{}, errors.New("workspace reset CAS is required for an artifact target")
	}
	envelope := request.GetEnvelope()
	fence := envelope.GetAuthority().GetFence()
	if target.BaseVersionID != fence.GetBaseWorkspaceVersionId() {
		return WorkspaceReset{}, errors.New("workspace reset target does not match the admitted base version")
	}
	expectedFingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationResetKind, workspace.FinalizationRequest{
		OperationID: envelope.GetOperationId(),
		Fence:       executorFinalizationFence(fence),
		Target:      target,
	})
	if err != nil || expectedFingerprint != envelope.GetRequestFingerprint() {
		return WorkspaceReset{}, errors.New("workspace reset request fingerprint is invalid")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return WorkspaceReset{}, fmt.Errorf("open workspace reset stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceReset,
		RunID:            fence.GetRunId(),
		WorkspaceID:      fence.GetWorkspaceId(),
		WorkspaceMountID: fence.GetWorkspaceMountId(),
		OperationID:      envelope.GetOperationId(),
	}, 0); err != nil {
		return WorkspaceReset{}, fmt.Errorf("write workspace reset header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return WorkspaceReset{}, fmt.Errorf("write workspace reset request: %w", err)
	}
	var artifactResult chan error
	if target.Kind == workspace.ResetTargetArtifact {
		artifactResult = make(chan error, 1)
		go func() {
			err := writeWorkspaceResetArtifact(ctx, stream, store, envelope, target)
			if err != nil {
				_ = stream.Close()
			}
			artifactResult <- err
		}()
	}
	var response workspacev0.ResetWorkspaceResponse
	readErr := readWorkspaceControlResponse(ctx, stream, &response)
	if readErr != nil || strings.TrimSpace(response.GetError()) != "" {
		_ = stream.Close()
	}
	if artifactResult != nil {
		if err := <-artifactResult; err != nil {
			return WorkspaceReset{}, err
		}
	}
	if readErr != nil {
		return WorkspaceReset{}, fmt.Errorf("read workspace reset response: %w", readErr)
	}
	if strings.TrimSpace(response.GetError()) != "" {
		return WorkspaceReset{}, fmt.Errorf("workspace reset failed: %s", response.GetError())
	}
	if response.GetReceipt() == nil ||
		response.GetReceipt().GetOperationId() != envelope.GetOperationId() ||
		response.GetReceipt().GetRequestFingerprint() != envelope.GetRequestFingerprint() ||
		!proto.Equal(response.GetReceipt().GetFence(), fence) ||
		!proto.Equal(response.GetTarget(), request.GetTarget()) {
		return WorkspaceReset{}, errors.New("workspace reset receipt does not match the request")
	}
	return WorkspaceReset{Receipt: response.GetReceipt(), Target: target}, nil
}

func writeWorkspaceResetArtifact(ctx context.Context, stream io.Writer, store cas.Reader, envelope *workspacev0.WorkspaceFinalizationEnvelope, target workspace.ResetTarget) error {
	artifact := target.Artifact
	object, err := store.Stat(ctx, artifact.Digest)
	if err != nil {
		return fmt.Errorf("stat workspace reset artifact: %w", err)
	}
	if object.Digest != artifact.Digest || object.SizeBytes != artifact.SizeBytes || object.MediaType != artifact.MediaType {
		return errors.New("workspace reset CAS object does not match its target")
	}
	body, err := store.Get(ctx, artifact.Digest)
	if err != nil {
		return fmt.Errorf("open workspace reset artifact: %w", err)
	}
	defer body.Close()
	digest := artifact.Digest
	entryCount := artifact.EntryCount
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceArtifact,
		WorkspaceID:      envelope.GetAuthority().GetFence().GetWorkspaceId(),
		WorkspaceMountID: envelope.GetAuthority().GetFence().GetWorkspaceMountId(),
		OperationID:      envelope.GetOperationId(),
		BodyDigest:       &digest,
		EntryCount:       &entryCount,
	}, uint64(artifact.SizeBytes)); err != nil {
		return fmt.Errorf("write workspace reset artifact header: %w", err)
	}
	hash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(stream, hash), body, artifact.SizeBytes)
	if err != nil {
		return fmt.Errorf("write workspace reset artifact: %w", err)
	}
	if written != artifact.SizeBytes {
		return errors.New("workspace reset artifact stream ended early")
	}
	var extra [1]byte
	if count, err := body.Read(extra[:]); err != io.EOF || count != 0 {
		if err == nil {
			return errors.New("workspace reset CAS object exceeds its declared size")
		}
		return fmt.Errorf("finish workspace reset artifact: %w", err)
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != artifact.Digest {
		return errors.New("workspace reset CAS object digest does not match its target")
	}
	return nil
}
