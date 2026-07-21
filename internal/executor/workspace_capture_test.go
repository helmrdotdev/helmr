package executor

import (
	"context"
	"net"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestCaptureWorkspaceOnSessionStoresVerifiedArtifact(t *testing.T) {
	artifact, cleanup, err := workspace.CreateEmptyWorkspaceArtifact(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	authority := &workspacev0.WorkspaceRunAuthority{
		Fence: &workspacev0.WorkspaceAuthorityFence{
			WorkspaceId:            "workspace-1",
			WorkspaceMountId:       "mount-1",
			RunId:                  "run-1",
			BaseWorkspaceVersionId: "version-1",
		},
	}
	operationID := "11111111-1111-4111-8111-111111111111"
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationCaptureKind, workspace.FinalizationRequest{
		OperationID: operationID,
		Fence:       executorFinalizationFence(authority.GetFence()),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.CaptureWorkspaceRequest{Envelope: &workspacev0.WorkspaceFinalizationEnvelope{
		OperationId:        operationID,
		RequestFingerprint: fingerprint,
		Authority:          authority,
	}}
	host, guest := net.Pipe()
	defer guest.Close()
	session := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	serverResult := make(chan error, 1)
	go func() {
		header, bodyLength, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			serverResult <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceCapture || bodyLength != 0 {
			serverResult <- errUnexpectedWorkspaceCapture
			return
		}
		var received workspacev0.CaptureWorkspaceRequest
		if err := frameio.ReadProtoFrame(guest, &received); err != nil {
			serverResult <- err
			return
		}
		response := &workspacev0.CaptureWorkspaceResponse{
			Receipt: &workspacev0.WorkspaceFinalizationReceipt{
				OperationId:        operationID,
				RequestFingerprint: fingerprint,
				Fence:              proto.Clone(authority.GetFence()).(*workspacev0.WorkspaceAuthorityFence),
			},
			Tree: &workspacev0.WorkspaceTreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
			Artifact: &workspacev0.WorkspaceArtifact{
				Digest:     artifact.Digest,
				MediaType:  artifact.MediaType,
				Encoding:   artifact.Encoding,
				SizeBytes:  uint64(artifact.SizeBytes),
				EntryCount: uint32(artifact.EntryCount),
			},
		}
		if err := frameio.WriteProtoFrame(guest, response); err != nil {
			serverResult <- err
			return
		}
		entryCount := artifact.EntryCount
		serverResult <- wire.WriteFileFrameWithMetadata(guest, wire.StreamHeader{
			Type:        wire.StreamTypeWorkspaceArtifact,
			WorkspaceID: "workspace-1",
			OperationID: operationID,
			EntryCount:  &entryCount,
		}, artifact.Path, artifact.Digest, artifact.SizeBytes)
	}()
	store := &fakeCAS{}
	capture, err := captureWorkspaceOnSession(context.Background(), session, store, request)
	if err != nil {
		t.Fatal(err)
	}
	if capture.ReportedTree.Digest != workspace.CanonicalEmptyTreeDigest || capture.Artifact.Digest != artifact.Digest {
		t.Fatalf("Capture = %+v", capture)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestCaptureWorkspaceOnSessionRejectsInvalidTreeIdentity(t *testing.T) {
	authority, operationID, fingerprint, request := testWorkspaceCaptureEnvelope(t)
	host, guest := net.Pipe()
	defer guest.Close()
	session := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	serverResult := make(chan error, 1)
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(guest); err != nil {
			serverResult <- err
			return
		}
		var received workspacev0.CaptureWorkspaceRequest
		if err := frameio.ReadProtoFrame(guest, &received); err != nil {
			serverResult <- err
			return
		}
		serverResult <- frameio.WriteProtoFrame(guest, &workspacev0.CaptureWorkspaceResponse{
			Receipt: &workspacev0.WorkspaceFinalizationReceipt{
				OperationId:        operationID,
				RequestFingerprint: fingerprint,
				Fence:              proto.Clone(authority.GetFence()).(*workspacev0.WorkspaceAuthorityFence),
			},
			Tree: &workspacev0.WorkspaceTreeIdentity{
				Digest:    workspace.CanonicalEmptyTreeDigest,
				SizeBytes: -1,
			},
		})
	}()
	if _, err := captureWorkspaceOnSession(context.Background(), session, &fakeCAS{}, request); err == nil {
		t.Fatal("invalid Workspace tree identity was accepted")
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func testWorkspaceCaptureEnvelope(t *testing.T) (*workspacev0.WorkspaceRunAuthority, string, string, *workspacev0.CaptureWorkspaceRequest) {
	t.Helper()
	authority := &workspacev0.WorkspaceRunAuthority{
		Fence: &workspacev0.WorkspaceAuthorityFence{
			WorkspaceId:            "workspace-1",
			WorkspaceMountId:       "mount-1",
			RunId:                  "run-1",
			BaseWorkspaceVersionId: "version-1",
		},
	}
	operationID := "11111111-1111-4111-8111-111111111111"
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationCaptureKind, workspace.FinalizationRequest{
		OperationID: operationID,
		Fence:       executorFinalizationFence(authority.GetFence()),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.CaptureWorkspaceRequest{Envelope: &workspacev0.WorkspaceFinalizationEnvelope{
		OperationId:        operationID,
		RequestFingerprint: fingerprint,
		Authority:          authority,
	}}
	return authority, operationID, fingerprint, request
}

var errUnexpectedWorkspaceCapture = &workspaceCaptureTestError{}

type workspaceCaptureTestError struct{}

func (*workspaceCaptureTestError) Error() string { return "unexpected Workspace Capture request" }
