package guestd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestWorkspaceMaterializeRestoresArtifactAndAuthorizesNoop(t *testing.T) {
	tempRoot := t.TempDir()
	root := filepath.Join(tempRoot, "workspace-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(root, tempRoot, tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	imagePath := filepath.Join(tempRoot, "sandbox.oci.tar")
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	imageDigest := sha256sum.DigestBytes(image)
	mountPath := "/workspace"
	registry := newWorkspaceOperationRegistry()
	materializeClient, materializeServer := net.Pipe()
	defer materializeClient.Close()
	defer materializeServer.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceMaterializeConnection(context.Background(), materializeServer, registry)
	}()
	if err := transport.WriteProtoFrame(materializeClient, &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			MaterializationId: "mat-1",
			WorkspaceId:       "workspace-1",
			ChannelToken:      "channel-token",
			FencingGeneration: 1,
		},
		MountPath:     mountPath,
		BaseVersionId: "version-1",
		BaseArtifact: &workspacev0.WorkspaceArtifact{
			Digest:     artifact.Digest,
			MediaType:  artifact.MediaType,
			Encoding:   artifact.Encoding,
			SizeBytes:  uint64(artifact.SizeBytes),
			EntryCount: uint32(artifact.EntryCount),
		},
		SandboxArtifact: &workspacev0.WorkspaceArtifact{
			Digest:    imageDigest,
			MediaType: api.SandboxImageArtifactMediaType,
			Encoding:  "oci-tar",
			SizeBytes: uint64(len(image)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteFileFrame(materializeClient, transport.StreamHeader{
		Type:        transport.StreamTypeRunImage,
		WorkspaceID: "workspace-1",
	}, imagePath); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteFileFrame(materializeClient, transport.StreamHeader{
		Type:        transport.StreamTypeWorkspaceArtifact,
		WorkspaceID: "workspace-1",
	}, artifact.Path); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := transport.ReadProtoFrame(materializeClient, &response); err != nil {
		t.Fatal(err)
	}
	if response.State != "running" || response.GuestdChannelTokenHash != sha256sum.HexBytes([]byte("channel-token")) {
		t.Fatalf("response state=%q guestd_channel_token_hash=%q", response.State, response.GuestdChannelTokenHash)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	registry.mu.RLock()
	workspaceRoot := registry.entries["mat-1"].workspaceRoot
	registry.mu.RUnlock()
	body, err := os.ReadFile(filepath.Join(workspaceRoot, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("restored file = %q", body)
	}

	operationClient, operationServer := net.Pipe()
	defer operationClient.Close()
	defer operationServer.Close()
	errCh = make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceOperationConnection(context.Background(), operationServer, registry)
	}()
	if err := transport.WriteProtoFrame(operationClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-1",
			MaterializationId:          "mat-1",
			WorkspaceId:                "workspace-1",
			ChannelToken:               "channel-token",
			FencingGeneration:          1,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         workspaceOperationRequestFingerprint("noop", `{}`),
		},
		OperationKind: "noop",
		RequestJson:   `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	var result workspacev0.WorkspaceOperationResult
	if err := transport.ReadProtoFrame(operationClient, &result); err != nil {
		t.Fatal(err)
	}
	if result.ResultJson != `{"ok":true}` || result.ErrorJson != "" {
		t.Fatalf("operation result_json=%q error_json=%q", result.ResultJson, result.ErrorJson)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	mismatchClient, mismatchServer := net.Pipe()
	defer mismatchClient.Close()
	defer mismatchServer.Close()
	errCh = make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceOperationConnection(context.Background(), mismatchServer, registry)
	}()
	if err := transport.WriteProtoFrame(mismatchClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-mismatch",
			MaterializationId:          "mat-1",
			WorkspaceId:                "workspace-other",
			ChannelToken:               "channel-token",
			FencingGeneration:          1,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         workspaceOperationRequestFingerprint("noop", `{}`),
		},
		OperationKind: "noop",
		RequestJson:   `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "channel token or fencing generation is invalid") {
		t.Fatalf("workspace mismatch err = %v, want channel token/fencing rejection", err)
	}

	advanceFenceClient, advanceFenceServer := net.Pipe()
	defer advanceFenceClient.Close()
	defer advanceFenceServer.Close()
	errCh = make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceOperationConnection(context.Background(), advanceFenceServer, registry)
	}()
	if err := transport.WriteProtoFrame(advanceFenceClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-advance-fence",
			MaterializationId:          "mat-1",
			WorkspaceId:                "workspace-1",
			ChannelToken:               "channel-token",
			FencingGeneration:          2,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         workspaceOperationRequestFingerprint("noop", `{}`),
		},
		OperationKind: "noop",
		RequestJson:   `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := transport.ReadProtoFrame(advanceFenceClient, &result); err != nil {
		t.Fatal(err)
	}
	if result.ResultJson != `{"ok":true}` || result.ErrorJson != "" {
		t.Fatalf("advance fencing operation result_json=%q error_json=%q", result.ResultJson, result.ErrorJson)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	staleFenceClient, staleFenceServer := net.Pipe()
	defer staleFenceClient.Close()
	defer staleFenceServer.Close()
	errCh = make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceOperationConnection(context.Background(), staleFenceServer, registry)
	}()
	if err := transport.WriteProtoFrame(staleFenceClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-stale-fence",
			MaterializationId:          "mat-1",
			WorkspaceId:                "workspace-1",
			ChannelToken:               "channel-token",
			FencingGeneration:          1,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         workspaceOperationRequestFingerprint("noop", `{}`),
		},
		OperationKind: "noop",
		RequestJson:   `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "channel token or fencing generation is invalid") {
		t.Fatalf("stale fencing err = %v, want channel token/fencing rejection", err)
	}
}

func TestWorkspaceOperationRegistryDefersRetiredCleanupUntilRelease(t *testing.T) {
	tempRoot := t.TempDir()
	oldRoot := filepath.Join(tempRoot, "old")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.register("mat-1", workspaceMaterializationEntry{
		workspaceID:       "workspace-1",
		channelToken:      "token-1",
		fencingGeneration: 1,
		workspaceRoot:     filepath.Join(oldRoot, "workspace"),
		cleanup:           func() { _ = os.RemoveAll(oldRoot) },
	})
	_, release, ok := registry.acquire("mat-1", "workspace-1", "token-1", 1)
	if !ok {
		t.Fatal("expected registry acquire")
	}
	registry.register("mat-1", workspaceMaterializationEntry{
		workspaceID:       "workspace-1",
		channelToken:      "token-2",
		fencingGeneration: 2,
		workspaceRoot:     filepath.Join(tempRoot, "new", "workspace"),
		cleanup:           func() {},
	})
	if _, err := os.Stat(oldRoot); err != nil {
		t.Fatalf("old workspace root was cleaned while acquired: %v", err)
	}
	release()
	if _, err := os.Stat(oldRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old workspace root after release err = %v, want not exist", err)
	}
}
