package executor

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestResetWorkspaceFromVerifiedArtifact(t *testing.T) {
	root := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := workspace.InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(root, t.TempDir(), filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	body, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	target, err := workspace.ArtifactResetTarget("version-1", tree, workspace.ArtifactIdentity{
		Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
		SizeBytes: artifact.SizeBytes, EntryCount: artifact.EntryCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	authority := &workspacev0.WorkspaceRunAuthority{Fence: &workspacev0.WorkspaceAuthorityFence{
		WorkspaceId: "workspace-1", WorkspaceMountId: "mount-1", RunId: "run-1", BaseWorkspaceVersionId: "version-1",
	}}
	operationID := "11111111-1111-4111-8111-111111111111"
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationResetKind, workspace.FinalizationRequest{
		OperationID: operationID, Fence: executorFinalizationFence(authority.GetFence()), Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.ResetWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceFinalizationEnvelope{
			OperationId: operationID, RequestFingerprint: fingerprint, Authority: authority,
		},
		Target: workspace.ResetTargetProto(target),
	}
	host, guest := net.Pipe()
	defer guest.Close()
	session := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	serverResult := make(chan error, 1)
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(guest); err != nil {
			serverResult <- err
			return
		}
		var received workspacev0.ResetWorkspaceRequest
		if err := frameio.ReadProtoFrame(guest, &received); err != nil {
			serverResult <- err
			return
		}
		header, bodyLength, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			serverResult <- err
			return
		}
		receivedBody, err := io.ReadAll(io.LimitReader(guest, int64(bodyLength)))
		if err != nil {
			serverResult <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceArtifact || !bytes.Equal(receivedBody, body) {
			serverResult <- errUnexpectedWorkspaceCapture
			return
		}
		serverResult <- frameio.WriteProtoFrame(guest, &workspacev0.ResetWorkspaceResponse{
			Receipt: &workspacev0.WorkspaceFinalizationReceipt{
				OperationId: operationID, RequestFingerprint: fingerprint,
				Fence: proto.Clone(authority.GetFence()).(*workspacev0.WorkspaceAuthorityFence),
			},
			Target: workspace.ResetTargetProto(target),
		})
	}()
	store := &fakeCAS{objects: map[string][]byte{}}
	store.put(workspace.ArtifactMediaType, body)
	reset, err := resetWorkspaceOnSession(context.Background(), session, store, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reset.Target, target) || reset.Receipt.GetOperationId() != operationID {
		t.Fatalf("Workspace Reset = %+v", reset)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}
