package guestd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func testWorkspaceOperationFingerprint(t *testing.T, operationKind string, requestJSON string) string {
	t.Helper()
	fingerprint, err := wire.RequestFingerprint(operationKind, []byte(requestJSON))
	if err != nil {
		t.Fatal(fmt.Errorf("workspace operation fingerprint: %w", err))
	}
	return fingerprint
}

func TestWorkspaceMaterializeRestoresArtifactAndAuthorizesPrimitiveOperation(t *testing.T) {
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
	imagePath := filepath.Join(tempRoot, "workspace-image.oci.tar")
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
		errCh <- handleWorkspaceMaterializeConnection(context.Background(), materializeServer, slogDiscard(), registry)
	}()
	if err := frameio.WriteProtoFrame(materializeClient, &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  "mat-1",
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
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    imageDigest,
			MediaType: workspaceImageMediaType,
			Encoding:  workspaceImageEncoding,
			SizeBytes: uint64(len(image)),
		},
		RuntimeInstanceId: "runtime-instance-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(materializeClient, wire.StreamHeader{
		Type:        wire.StreamTypeRunImage,
		WorkspaceID: "workspace-1",
	}, imagePath); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(materializeClient, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceArtifact,
		WorkspaceID: "workspace-1",
	}, artifact.Path); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := frameio.ReadProtoFrame(materializeClient, &response); err != nil {
		t.Fatal(err)
	}
	if response.State != "running" || response.GuestdChannelTokenHash != sha256sum.HexBytes([]byte("channel-token")) {
		t.Fatalf("response state=%q guestd_channel_token_hash=%q", response.State, response.GuestdChannelTokenHash)
	}
	if !workspaceMountPhaseNames(response.Phases, "guest_workspace_image_restore", "guest_workspace_artifact_restore", "guest_register") {
		t.Fatalf("response phases = %+v", response.Phases)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	registry.mu.RLock()
	entry := registry.entries["mat-1"]
	workspaceRoot := entry.workspaceRoot
	registry.mu.RUnlock()
	stateRelativeToImage, err := filepath.Rel(entry.imageRoot, entry.finalizationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if stateRelativeToImage != ".." && !strings.HasPrefix(stateRelativeToImage, ".."+string(os.PathSeparator)) {
		t.Fatalf("Workspace finalization state %q is visible inside image root %q", entry.finalizationRoot, entry.imageRoot)
	}
	if tempRoot := os.Getenv("HELMR_GUESTD_TMPDIR"); tempRoot != "" && !strings.HasPrefix(workspaceRoot, tempRoot+string(os.PathSeparator)) {
		t.Fatalf("workspace root = %q, want under HELMR_GUESTD_TMPDIR %q", workspaceRoot, tempRoot)
	}
	body, err := os.ReadFile(filepath.Join(workspaceRoot, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("restored file = %q", body)
	}
	operationKind := "ResizePty"
	operationRequest := `{"pty_id":"pty-1","cols":80,"rows":24}`

	operationClient, operationServer := net.Pipe()
	defer operationClient.Close()
	defer operationServer.Close()
	errCh = make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceOperationConnection(context.Background(), operationServer, registry)
	}()
	if err := frameio.WriteProtoFrame(operationClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-1",
			WorkspaceMountId:           "mat-1",
			WorkspaceId:                "workspace-1",
			ChannelToken:               "channel-token",
			FencingGeneration:          1,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         testWorkspaceOperationFingerprint(t, operationKind, operationRequest),
		},
		OperationKind: operationKind,
		RequestJson:   operationRequest,
	}); err != nil {
		t.Fatal(err)
	}
	var result workspacev0.WorkspaceOperationResult
	if err := frameio.ReadProtoFrame(operationClient, &result); err != nil {
		t.Fatal(err)
	}
	if result.ResultJson != "" || !strings.Contains(result.ErrorJson, `workspace pty \"pty-1\" is not open`) {
		t.Fatalf("operation result_json=%q error_json=%q, want authorized primitive handler error", result.ResultJson, result.ErrorJson)
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
	if err := frameio.WriteProtoFrame(mismatchClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-mismatch",
			WorkspaceMountId:           "mat-1",
			WorkspaceId:                "workspace-other",
			ChannelToken:               "channel-token",
			FencingGeneration:          1,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         testWorkspaceOperationFingerprint(t, operationKind, operationRequest),
		},
		OperationKind: operationKind,
		RequestJson:   operationRequest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := frameio.ReadProtoFrame(mismatchClient, &result); err != nil {
		t.Fatal(err)
	}
	if result.ResultJson != "" || !strings.Contains(result.ErrorJson, "channel token or fencing generation is invalid") {
		t.Fatalf("workspace mismatch result_json=%q error_json=%q, want channel token/fencing rejection", result.ResultJson, result.ErrorJson)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	advanceFenceClient, advanceFenceServer := net.Pipe()
	defer advanceFenceClient.Close()
	defer advanceFenceServer.Close()
	errCh = make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceOperationConnection(context.Background(), advanceFenceServer, registry)
	}()
	if err := frameio.WriteProtoFrame(advanceFenceClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-advance-fence",
			WorkspaceMountId:           "mat-1",
			WorkspaceId:                "workspace-1",
			ChannelToken:               "channel-token",
			FencingGeneration:          2,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         testWorkspaceOperationFingerprint(t, operationKind, operationRequest),
		},
		OperationKind: operationKind,
		RequestJson:   operationRequest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := frameio.ReadProtoFrame(advanceFenceClient, &result); err != nil {
		t.Fatal(err)
	}
	if result.ResultJson != "" || !strings.Contains(result.ErrorJson, `workspace pty \"pty-1\" is not open`) {
		t.Fatalf("advance fencing operation result_json=%q error_json=%q, want authorized primitive handler error", result.ResultJson, result.ErrorJson)
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
	if err := frameio.WriteProtoFrame(staleFenceClient, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                "op-stale-fence",
			WorkspaceMountId:           "mat-1",
			WorkspaceId:                "workspace-1",
			ChannelToken:               "channel-token",
			FencingGeneration:          1,
			OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(),
			RequestFingerprint:         testWorkspaceOperationFingerprint(t, operationKind, operationRequest),
		},
		OperationKind: operationKind,
		RequestJson:   operationRequest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := frameio.ReadProtoFrame(staleFenceClient, &result); err != nil {
		t.Fatal(err)
	}
	if result.ResultJson != "" || !strings.Contains(result.ErrorJson, "channel token or fencing generation is invalid") {
		t.Fatalf("stale fencing result_json=%q error_json=%q, want channel token/fencing rejection", result.ResultJson, result.ErrorJson)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceRuntimePrepareUsesWorkspaceImageAndRuntimeInstanceID(t *testing.T) {
	tempRoot := t.TempDir()
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	imagePath := filepath.Join(tempRoot, "workspace-image.oci.tar")
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	imageDigest := sha256sum.DigestBytes(image)
	registry := newWorkspaceOperationRegistry()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceRuntimePrepareConnection(context.Background(), server, slogDiscard(), registry)
	}()
	const runtimeInstanceID = " runtime-instance-1 "
	if err := frameio.WriteProtoFrame(client, &workspacev0.PrepareWorkspaceRuntimeRequest{
		RuntimeInstanceId: runtimeInstanceID,
		MountPath:         "/workspace",
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    imageDigest,
			MediaType: workspaceImageMediaType,
			Encoding:  workspaceImageEncoding,
			SizeBytes: uint64(len(image)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(client, wire.StreamHeader{Type: wire.StreamTypeRunImage}, imagePath); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.PrepareWorkspaceRuntimeResponse
	if err := frameio.ReadProtoFrame(client, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetState() != "prepared" || response.GetRuntimeInstanceId() != runtimeInstanceID {
		t.Fatalf("response state=%q runtime_instance_id=%q", response.GetState(), response.GetRuntimeInstanceId())
	}
	if !workspaceMountPhaseNames(response.GetPhases(), "guest_workspace_image_restore", "guest_runtime_user_resolve", "guest_workspace_root_resolve") {
		t.Fatalf("response phases = %+v", response.GetPhases())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.takePreparedRuntime("runtime-instance-other", imageDigest, "/workspace"); ok {
		t.Fatal("prepared runtime accepted a different runtime_instance_id")
	}
	if _, ok := registry.takePreparedRuntime(strings.TrimSpace(runtimeInstanceID), imageDigest, "/workspace"); ok {
		t.Fatal("prepared runtime normalized an opaque runtime_instance_id")
	}
	prepared, ok := registry.takePreparedRuntime(runtimeInstanceID, imageDigest, "/workspace")
	if !ok {
		t.Fatal("prepared runtime did not accept the matching runtime_instance_id")
	}
	prepared.cleanup()
}

func TestWorkspaceImageContractIsExact(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		encoding  string
		want      string
	}{
		{name: "old media type", mediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar", encoding: workspaceImageEncoding, want: "media_type"},
		{name: "padded media type", mediaType: " " + workspaceImageMediaType, encoding: workspaceImageEncoding, want: "media_type"},
		{name: "wrong encoding", mediaType: workspaceImageMediaType, encoding: "tar", want: "encoding"},
		{name: "padded encoding", mediaType: workspaceImageMediaType, encoding: workspaceImageEncoding + " ", want: "encoding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspaceImage := &workspacev0.WorkspaceArtifact{
				Digest:    "sha256:image",
				MediaType: tt.mediaType,
				Encoding:  tt.encoding,
				SizeBytes: 1,
			}
			_, _, err := restorePreparedWorkspaceRuntime(bytes.NewReader(nil), &workspacev0.PrepareWorkspaceRuntimeRequest{
				RuntimeInstanceId: "runtime-instance-1",
				MountPath:         "/workspace",
				WorkspaceImage:    workspaceImage,
			}, slogDiscard())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("prepare error = %v, want %s rejection", err, tt.want)
			}
			_, _, err = restoreWorkspaceMount(bytes.NewReader(nil), &workspacev0.MaterializeWorkspaceRequest{
				Envelope:          &workspacev0.WorkspaceOperationEnvelope{},
				MountPath:         "/workspace",
				BaseVersionId:     "version-1",
				RuntimeInstanceId: "runtime-instance-1",
				BaseArtifact: &workspacev0.WorkspaceArtifact{
					Digest:     "sha256:base",
					MediaType:  workspace.ArtifactMediaType,
					Encoding:   workspace.ArtifactEncoding,
					SizeBytes:  1,
					EntryCount: 0,
				},
				WorkspaceImage: workspaceImage,
			}, slogDiscard(), newWorkspaceOperationRegistry())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("materialize error = %v, want %s rejection", err, tt.want)
			}
		})
	}
	_, _, err := restorePreparedWorkspaceRuntime(bytes.NewReader(nil), &workspacev0.PrepareWorkspaceRuntimeRequest{
		RuntimeInstanceId: "   ",
		MountPath:         "/workspace",
	}, slogDiscard())
	if err == nil || !strings.Contains(err.Error(), "runtime_instance_id is required") {
		t.Fatalf("whitespace runtime_instance_id error = %v", err)
	}
}

func TestWorkspaceMaterializeWithoutBaseArtifactInitializesEmptyRoot(t *testing.T) {
	tempRoot := t.TempDir()
	image := ociTar(t, []ociTestLayer{{
		mediaType: "application/vnd.oci.image.layer.v1.tar",
		body:      tarBytes(t, map[string]string{"workspace/from-image.txt": "remove me"}),
	}}, []byte(`{"Config":{}}`))
	imagePath := filepath.Join(tempRoot, "workspace-image.oci.tar")
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceMaterializeConnection(context.Background(), server, slogDiscard(), registry)
	}()
	if err := frameio.WriteProtoFrame(client, &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  "mat-empty",
			WorkspaceId:       "workspace-empty",
			ChannelToken:      "channel-token",
			FencingGeneration: 1,
		},
		MountPath:     "/workspace",
		BaseVersionId: "version-zero",
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    sha256sum.DigestBytes(image),
			MediaType: workspaceImageMediaType,
			Encoding:  workspaceImageEncoding,
			SizeBytes: uint64(len(image)),
		},
		RuntimeInstanceId: "runtime-instance-empty",
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(client, wire.StreamHeader{
		Type:        wire.StreamTypeRunImage,
		WorkspaceID: "workspace-empty",
	}, imagePath); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := frameio.ReadProtoFrame(client, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetState() != "running" {
		t.Fatalf("response state = %q", response.GetState())
	}
	if !workspaceMountPhaseNames(response.GetPhases(), "guest_workspace_image_restore", "guest_workspace_empty_root_init", "guest_register") {
		t.Fatalf("response phases = %+v", response.GetPhases())
	}
	if workspaceMountPhaseNames(response.GetPhases(), "guest_workspace_artifact_restore") {
		t.Fatalf("base-artifact-free response unexpectedly restored an artifact: %+v", response.GetPhases())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	registry.mu.RLock()
	workspaceRoot := registry.entries["mat-empty"].workspaceRoot
	registry.mu.RUnlock()
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty workspace root entries = %v", entries)
	}
}

func TestWorkspaceMaterializeReturnsFailureResponse(t *testing.T) {
	registry := newWorkspaceOperationRegistry()
	materializeClient, materializeServer := net.Pipe()
	defer materializeClient.Close()
	defer materializeServer.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceMaterializeConnection(context.Background(), materializeServer, slogDiscard(), registry)
	}()
	if err := frameio.WriteProtoFrame(materializeClient, &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  "mat-1",
			WorkspaceId:       "workspace-1",
			ChannelToken:      "channel-token",
			FencingGeneration: 1,
		},
		MountPath:         "relative",
		BaseVersionId:     "version-1",
		RuntimeInstanceId: "runtime-instance-1",
	}); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := frameio.ReadProtoFrame(materializeClient, &response); err != nil {
		t.Fatal(err)
	}
	if response.State != "failed" {
		t.Fatalf("response state = %q, want failed", response.State)
	}
	if got := testWorkspaceMountPhaseError(response.Phases); !strings.Contains(got, "mount_path") {
		t.Fatalf("phase error = %q, want mount_path", got)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "mount_path") {
		t.Fatalf("handler error = %v, want mount_path", err)
	}
}

func testWorkspaceMountPhaseError(phases []*workspacev0.WorkspaceMountPhase) string {
	for i := len(phases) - 1; i >= 0; i-- {
		if phases[i] != nil && strings.TrimSpace(phases[i].GetError()) != "" {
			return strings.TrimSpace(phases[i].GetError())
		}
	}
	return ""
}

func workspaceMountPhaseNames(phases []*workspacev0.WorkspaceMountPhase, expected ...string) bool {
	seen := map[string]bool{}
	for _, phase := range phases {
		if phase == nil {
			continue
		}
		seen[phase.Name] = true
	}
	for _, name := range expected {
		if !seen[name] {
			return false
		}
	}
	return true
}

func TestWorkspaceOperationRegistryDefersRetiredCleanupUntilRelease(t *testing.T) {
	tempRoot := t.TempDir()
	oldRoot := filepath.Join(tempRoot, "old")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.register("mat-1", &workspaceMountEntry{
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
	registry.register("mat-1", &workspaceMountEntry{
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
