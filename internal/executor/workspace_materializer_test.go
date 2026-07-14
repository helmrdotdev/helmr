package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/localcache"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func testWorkspaceMountArtifacts(t *testing.T) (*fakeCAS, api.WorkerWorkspaceMount) {
	t.Helper()
	store := &fakeCAS{objects: map[string][]byte{}}
	imageObject, err := store.Put(context.Background(), api.SandboxImageArtifactMediaType, strings.NewReader("oci image"))
	if err != nil {
		t.Fatal(err)
	}
	workspaceArtifact, cleanup, err := workspace.CreateEmptyWorkspaceArtifact(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	file, err := os.Open(workspaceArtifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	workspaceObject, err := store.Put(context.Background(), workspaceArtifact.MediaType, file)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return store, api.WorkerWorkspaceMount{
		BaseVersionID:              "version-1",
		RuntimeInstanceID:          "runtime-instance-1",
		RuntimeEpoch:               1,
		NetworkSlotID:              "network-slot-1",
		NetworkSlotGeneration:      1,
		RuntimeID:                  "runtime-1",
		SandboxImageArtifact:       api.CASObject{Digest: imageObject.Digest, SizeBytes: imageObject.SizeBytes, MediaType: imageObject.MediaType},
		SandboxImageArtifactFormat: "oci-tar",
		RootfsDigest:               "sha256:runtime-rootfs",
		ImageDigest:                imageObject.Digest,
		ImageFormat:                "oci-tar",
		WorkspaceArtifact: api.WorkerWorkspaceArtifact{
			Digest:     workspaceObject.Digest,
			MediaType:  workspaceObject.MediaType,
			Encoding:   workspace.ArtifactEncoding,
			SizeBytes:  workspaceObject.SizeBytes,
			EntryCount: int32(workspaceArtifact.EntryCount),
		},
		WorkspaceMountPath: "/workspace",
	}
}

func TestWorkspaceMaterializerRestoreCASObjectUsesLocalCache(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	cacheDir := t.TempDir()
	tempDir := t.TempDir()
	materializer := WorkspaceMaterializer{
		CAS:              store,
		ArtifactCacheDir: cacheDir,
	}

	_, firstCleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "sandbox-image", workspaceMount.SandboxImageArtifact)
	if err != nil {
		t.Fatal(err)
	}
	firstCleanup()
	secondPath, secondCleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "sandbox-image", workspaceMount.SandboxImageArtifact)
	if err != nil {
		t.Fatal(err)
	}
	defer secondCleanup()
	body, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "oci image" {
		t.Fatalf("cached artifact body = %q", string(body))
	}
	if got := store.getCalls[workspaceMount.SandboxImageArtifact.Digest]; got != 1 {
		t.Fatalf("CAS Get calls = %d, want 1", got)
	}
}

func TestWorkspaceMaterializerRestoreCASObjectRefreshesInvalidLocalCache(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	cacheDir := t.TempDir()
	tempDir := t.TempDir()
	cachePath, err := artifactCachePath(cacheDir, workspaceMount.SandboxImageArtifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("bad image"), 0o644); err != nil {
		t.Fatal(err)
	}
	materializer := WorkspaceMaterializer{
		CAS:              store,
		ArtifactCacheDir: cacheDir,
	}

	path, cleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "sandbox-image", workspaceMount.SandboxImageArtifact)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "oci image" {
		t.Fatalf("refreshed artifact body = %q", string(body))
	}
	if got := store.getCalls[workspaceMount.SandboxImageArtifact.Digest]; got != 1 {
		t.Fatalf("CAS Get calls = %d, want 1", got)
	}
}

func TestEnforceArtifactCacheBudgetEvictsOldArtifacts(t *testing.T) {
	cacheDir := t.TempDir()
	oldPath, err := artifactCachePath(cacheDir, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	newPath, err := artifactCachePath(cacheDir, "sha256:2222222222222222222222222222222222222222222222222222222222222222")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, bytes.Repeat([]byte("o"), 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, bytes.Repeat([]byte("n"), 10), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	if _, err := localcache.EnforceByteLimit(filepath.Join(cacheDir, "sha256"), 10, cleanArtifactCachePreserveSet(map[string]bool{newPath: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old artifact stat err = %v, want not exist", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceMaterializerPassesRequestedResourcesToConnector(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.RequestedMilliCPU = 1500
	workspaceMount.RequestedMemoryMiB = 1024
	workspaceMount.RequestedDiskMiB = 4096
	workspaceMount.RequestedExecutionSlots = 1
	var requests []vm.MaterializeRequest
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{
			session:  &workspaceMaterializerTestSession{},
			requests: &requests,
		},
		CAS:     store,
		TempDir: t.TempDir(),
	}

	session, _, _, cleanup, _, _, err := materializer.materializeSession(context.Background(), &workspaceMount)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("session is nil")
	}
	if len(requests) != 1 {
		t.Fatalf("materialize requests = %d, want 1", len(requests))
	}
	if got := requests[0].Resources; got.MilliCPU != 1500 || got.MemoryMiB != 1024 || got.DiskMiB != 4096 || got.Slots != 1 {
		t.Fatalf("materialize resources = %+v", got)
	}
}

func TestWorkspaceMountPhaseErrorUsesLatestGuestError(t *testing.T) {
	got := workspaceMountPhaseError([]*workspacev0.WorkspaceMountPhase{
		{Name: "guest_sandbox_image_restore"},
		{Name: "guest_workspace_artifact_restore", Error: "extract workspace artifact: permission denied"},
	})
	if got != "guest_workspace_artifact_restore: extract workspace artifact: permission denied" {
		t.Fatalf("phase error = %q", got)
	}
}

func TestWorkspaceMaterializerColdStartsWhenPreparedRuntimeEntryMissing(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	materializer := WorkspaceMaterializer{
		Connector:   workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{}},
		CAS:         store,
		RuntimePool: NewPreparedRuntimePool(nil, nil, 1, nil),
	}

	session, sandboxPath, workspacePath, cleanup, key, usedPreparedRuntime, err := materializer.materializeSession(context.Background(), &workspaceMount)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if session != nil {
		_ = session.Close(context.Background())
	}
	if sandboxPath == "" || workspacePath == "" {
		t.Fatalf("materialized paths sandbox=%q workspace=%q, want both", sandboxPath, workspacePath)
	}
	if key != "" || usedPreparedRuntime {
		t.Fatalf("prepared runtime state key=%q used=%v, want cold workspaceMount", key, usedPreparedRuntime)
	}
}

func TestWorkspaceMaterializerColdMaterializeStartsIndependentWorkConcurrently(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	gate := newParallelStartGate()
	materializer := WorkspaceMaterializer{
		Connector: parallelStartConnector{
			gate:    gate,
			session: &workspaceMaterializerTestSession{},
		},
		CAS:     parallelStartCAS{Store: store, gate: gate, workspaceMount: workspaceMount},
		TempDir: t.TempDir(),
	}
	done := make(chan error, 1)
	go func() {
		session, sandboxImagePath, workspacePath, cleanup, _, _, err := materializer.materializeSession(context.Background(), &workspaceMount)
		if cleanup != nil {
			defer cleanup()
		}
		if session == nil && err == nil {
			err = errors.New("session is nil")
		}
		if strings.TrimSpace(sandboxImagePath) == "" && err == nil {
			err = errors.New("sandbox image path is empty")
		}
		if strings.TrimSpace(workspacePath) == "" && err == nil {
			err = errors.New("workspace path is empty")
		}
		done <- err
	}()
	wantStarted := map[string]bool{
		"connector":       true,
		"sandbox-image":   true,
		"workspace-image": true,
	}
	seen := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for len(seen) < len(wantStarted) {
		select {
		case label := <-gate.started:
			if wantStarted[label] {
				seen[label] = true
			}
		case <-timeout:
			t.Fatalf("cold workspaceMount work did not start concurrently; seen=%v", seen)
		}
	}
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("materializeSession did not finish after releasing concurrent work")
	}
}

func TestWorkspaceMaterializerDispatchesStartExecOperationToGuest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialClient, initialServer := net.Pipe()
	eventClient, eventServer := net.Pipe()
	operationClient, operationServer := net.Pipe()
	defer initialServer.Close()
	defer eventServer.Close()
	defer operationServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read materialize header: %v", err)
			return
		}
		if header.Type != wire.StreamTypeWorkspaceMaterialize {
			t.Errorf("materialize stream type = %s", header.Type)
			return
		}
		var request workspacev0.MaterializeWorkspaceRequest
		if err := frameio.ReadProtoFrame(initialServer, &request); err != nil {
			t.Errorf("read materialize request: %v", err)
			return
		}
		if got := request.GetEnvelope().GetChannelToken(); got != "channel-token" {
			t.Errorf("channel token = %q", got)
			return
		}
		if request.BaseVersionId != workspaceMount.BaseVersionID || request.MountPath != workspaceMount.WorkspaceMountPath || request.GetBaseArtifact().GetDigest() != workspaceMount.WorkspaceArtifact.Digest || request.GetSandboxArtifact().GetDigest() != workspaceMount.SandboxImageArtifact.Digest {
			t.Errorf("materialize request base_version_id=%q mount_path=%q base_digest=%q sandbox_digest=%q", request.BaseVersionId, request.MountPath, request.GetBaseArtifact().GetDigest(), request.GetSandboxArtifact().GetDigest())
			return
		}
		imageHeader, imageSize, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read sandbox image header: %v", err)
			return
		}
		if imageHeader.Type != wire.StreamTypeRunImage || imageHeader.WorkspaceID != workspaceMount.WorkspaceID || int64(imageSize) != workspaceMount.SandboxImageArtifact.SizeBytes {
			t.Errorf("sandbox image header = %+v size=%d", imageHeader, imageSize)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(imageSize)}); err != nil {
			t.Errorf("drain sandbox image: %v", err)
			return
		}
		artifactHeader, artifactSize, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read workspace artifact header: %v", err)
			return
		}
		if artifactHeader.Type != wire.StreamTypeWorkspaceArtifact || artifactHeader.WorkspaceID != workspaceMount.WorkspaceID || int64(artifactSize) != workspaceMount.WorkspaceArtifact.SizeBytes {
			t.Errorf("workspace artifact header = %+v size=%d", artifactHeader, artifactSize)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(artifactSize)}); err != nil {
			t.Errorf("drain workspace artifact: %v", err)
			return
		}
		_ = frameio.WriteProtoFrame(initialServer, &workspacev0.MaterializeWorkspaceResponse{
			State:                  "running",
			GuestdChannelTokenHash: sha256sum.HexBytes([]byte("channel-token")),
		})
	}()
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(eventServer)
		if err != nil {
			t.Errorf("read event header: %v", err)
			return
		}
		if header.Type != wire.StreamTypeWorkspaceEvents {
			t.Errorf("event header = %+v", header)
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := frameio.ReadProtoFrame(eventServer, &envelope); err != nil {
			t.Errorf("read event envelope: %v", err)
			return
		}
		if envelope.GetChannelToken() != "channel-token" || envelope.GetWorkspaceMountId() != "mat-1" {
			t.Errorf("event envelope = %+v", &envelope)
			return
		}
		<-ctx.Done()
	}()
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(operationServer)
		if err != nil {
			t.Errorf("read operation header: %v", err)
			return
		}
		if header.Type != wire.StreamTypeWorkspaceOperation || header.OperationID != "operation-1" {
			t.Errorf("operation header = %+v", header)
			return
		}
		var request workspacev0.WorkspaceOperationRequest
		if err := frameio.ReadProtoFrame(operationServer, &request); err != nil {
			t.Errorf("read operation request: %v", err)
			return
		}
		if request.OperationKind != "StartExec" || request.GetEnvelope().GetChannelToken() != "channel-token" || request.GetEnvelope().GetFencingToken() != "fence-1" || request.GetEnvelope().GetFencingGeneration() != 2 {
			t.Errorf("operation request kind=%q channel_token=%q fencing_token=%q fencing_generation=%d", request.OperationKind, request.GetEnvelope().GetChannelToken(), request.GetEnvelope().GetFencingToken(), request.GetEnvelope().GetFencingGeneration())
			return
		}
		_ = frameio.WriteProtoFrame(operationServer, &workspacev0.WorkspaceOperationResult{ResultJson: `{"ok":true}`})
	}()
	client := &workspaceMaterializerTestClient{
		cancel:      cancel,
		startErrors: []error{errors.New("temporary start error")},
		operation: &api.WorkerWorkspaceOperation{
			WorkspaceOperationResponse: api.WorkspaceOperationResponse{
				ID:                 "operation-1",
				OrgID:              "org-1",
				WorkspaceID:        "workspace-1",
				WorkspaceMountID:   "mat-1",
				OperationKind:      "StartExec",
				FencingToken:       "fence-1",
				FencingGeneration:  2,
				RequestFingerprint: testWorkspaceOperationFingerprint("StartExec", `{"exec_id":"exec-1","command":["echo","ok"],"detached":false}`),
				OperationExpiresAt: time.Now().Add(time.Hour),
				Request:            []byte(`{"exec_id":"exec-1","command":["echo","ok"],"detached":false}`),
			},
			ClaimToken: "claim-token",
		},
	}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{
			initial: initialClient,
			streams: []io.ReadWriteCloser{eventClient, operationClient},
		}},
		CAS:                  store,
		TempDir:              t.TempDir(),
		Heartbeat:            time.Hour,
		PollEvery:            time.Millisecond,
		CompleteErrorBackoff: time.Millisecond,
	}
	err := materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("materializer err = %v, want context canceled", err)
	}
	if string(client.completed.Result) != `{"ok":true}` || client.completed.OperationID != "operation-1" || client.completed.ClaimToken != "claim-token" {
		t.Fatalf("completed operation = %+v", client.completed)
	}
	if len(client.claims) == 0 || client.claims[0].OrgID != "org-1" || client.claims[0].WorkspaceMountID != "mat-1" {
		t.Fatalf("claim request = %+v", client.claims)
	}
	if len(client.starts) != 2 || client.starts[0].OperationID != "operation-1" || client.starts[1].OperationID != "operation-1" {
		t.Fatalf("start retries = %+v", client.starts)
	}
	if len(client.mounted) != 1 || client.mounted[0].OrgID != "org-1" || client.mounted[0].WorkspaceMountID != "mat-1" {
		t.Fatalf("mounted request = %+v", client.mounted)
	}
	if client.stops != 0 {
		t.Fatalf("stops = %d", client.stops)
	}
}

func TestWorkspaceMaterializerRejectsMismatchedClaimedOperation(t *testing.T) {
	materializer := WorkspaceMaterializer{}
	_, err := materializer.dispatchOperation(context.Background(), nil, api.WorkerWorkspaceMount{
		ID:          "mat-1",
		WorkspaceID: "workspace-1",
	}, api.WorkerWorkspaceOperation{
		WorkspaceOperationResponse: api.WorkspaceOperationResponse{
			ID:                 "operation-1",
			OrgID:              "org-1",
			WorkspaceID:        "workspace-2",
			WorkspaceMountID:   "mat-2",
			OperationKind:      "StartExec",
			RequestFingerprint: testWorkspaceOperationFingerprint("StartExec", `{"exec_id":"exec-1","command":["echo","ok"],"detached":false}`),
			OperationExpiresAt: time.Now().Add(time.Hour),
		},
		ClaimToken: "claim-token",
	})
	if err == nil {
		t.Fatal("expected mismatched claimed operation to fail before guest dispatch")
	}
}

func TestWorkspaceMaterializerStopWorkspaceGuestStoresCapturedArtifact(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	store := &fakeCAS{objects: map[string][]byte{}}
	body := []byte("captured workspace")
	object := store.put(workspace.ArtifactMediaType, body)
	session := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{client}}
	workspaceMount := api.WorkerWorkspaceMount{
		ID:                     "mat-1",
		WorkspaceID:            "workspace-1",
		GuestdChannelToken:     "channel-token",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte("channel-token")),
		FencingGeneration:      7,
	}
	done := make(chan error, 1)
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(server)
		if err != nil {
			done <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceStop || header.WorkspaceID != "workspace-1" {
			done <- fmt.Errorf("stop header = %+v", header)
			return
		}
		var request workspacev0.StopWorkspaceRequest
		if err := frameio.ReadProtoFrame(server, &request); err != nil {
			done <- err
			return
		}
		if !request.GetCaptureBeforeStop() || request.GetFinalizeStop() || request.GetEnvelope().GetChannelToken() != "channel-token" || request.GetEnvelope().GetFencingGeneration() != 7 {
			done <- fmt.Errorf("stop request = %+v", &request)
			return
		}
		if err := frameio.WriteProtoFrame(server, &workspacev0.StopWorkspaceResponse{
			State: "captured",
			CapturedArtifact: &workspacev0.WorkspaceArtifact{
				Digest:     object.Digest,
				MediaType:  object.MediaType,
				Encoding:   workspace.ArtifactEncoding,
				SizeBytes:  uint64(object.SizeBytes),
				EntryCount: 3,
			},
		}); err != nil {
			done <- err
			return
		}
		entryCount := 3
		if err := wire.WriteStreamFrameHeader(server, wire.StreamHeader{
			Type:        wire.StreamTypeWorkspaceArtifact,
			WorkspaceID: "workspace-1",
			BodyDigest:  &object.Digest,
			EntryCount:  &entryCount,
		}, uint64(len(body))); err != nil {
			done <- err
			return
		}
		_, err = server.Write(body)
		done <- err
	}()
	artifact, err := (WorkspaceMaterializer{CAS: store}).stopWorkspaceGuest(context.Background(), session, workspaceMount, workspaceMount.FencingGeneration, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if artifact.Digest != object.Digest || artifact.SizeBytes != object.SizeBytes || artifact.EntryCount != 3 {
		t.Fatalf("captured artifact = %+v, want digest=%s size=%d entries=3", artifact, object.Digest, object.SizeBytes)
	}
}

func TestWorkspaceMaterializerControlledStopUsesRenewedFencingGeneration(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.FencingGeneration = 7
	done := make(chan error, 1)
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(serverConn)
		if err != nil {
			done <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceStop || header.WorkspaceID != "workspace-1" {
			done <- fmt.Errorf("stop header = %+v", header)
			return
		}
		var request workspacev0.StopWorkspaceRequest
		if err := frameio.ReadProtoFrame(serverConn, &request); err != nil {
			done <- err
			return
		}
		if got := request.GetEnvelope().GetFencingGeneration(); got != 9 {
			done <- fmt.Errorf("stop fencing_generation = %d, want 9", got)
			return
		}
		if request.GetCaptureBeforeStop() || !request.GetFinalizeStop() {
			done <- fmt.Errorf("stop request = %+v", &request)
			return
		}
		done <- frameio.WriteProtoFrame(serverConn, &workspacev0.StopWorkspaceResponse{State: "stopped"})
	}()
	client := &workspaceMaterializerTestClient{}
	session := &workspaceMaterializerTestSession{
		streams: []io.ReadWriteCloser{clientConn},
	}
	err := (WorkspaceMaterializer{CAS: store}).stopControlledWorkspaceMount(context.Background(), session, workspaceMount, api.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9}, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.stops != 1 {
		t.Fatalf("stops = %d, want 1", client.stops)
	}
	if session.closed != 1 {
		t.Fatalf("session closes = %d, want 1 before reporting the mount stopped", session.closed)
	}
}

func TestWorkspaceMaterializerDoesNotReportStoppedWhenRuntimeCloseFails(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	done := make(chan error, 1)
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(serverConn); err != nil {
			done <- err
			return
		}
		var request workspacev0.StopWorkspaceRequest
		if err := frameio.ReadProtoFrame(serverConn, &request); err != nil {
			done <- err
			return
		}
		done <- frameio.WriteProtoFrame(serverConn, &workspacev0.StopWorkspaceResponse{State: "stopped"})
	}()
	client := &workspaceMaterializerTestClient{}
	session := &workspaceMaterializerTestSession{
		streams:  []io.ReadWriteCloser{clientConn},
		closeErr: errors.New("runtime cleanup failed"),
	}
	err := (WorkspaceMaterializer{CAS: store}).stopControlledWorkspaceMount(context.Background(), session, workspaceMount, api.WorkspaceMountResponse{State: "unmounting"}, client)
	if err == nil {
		t.Fatal("expected runtime close failure")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.stops != 0 {
		t.Fatalf("stops = %d, want 0", client.stops)
	}
	if len(client.failures) != 1 || !strings.Contains(string(client.failures[0].Error), "workspace_mount_runtime_close_failed") {
		t.Fatalf("failures = %+v", client.failures)
	}
}

func TestWorkspaceMaterializerControlledCleanStopFailureFailsWorkspaceMount(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	_ = serverConn.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	client := &workspaceMaterializerTestClient{}
	err := (WorkspaceMaterializer{CAS: store}).stopControlledWorkspaceMount(context.Background(), &workspaceMaterializerTestSession{
		streams: []io.ReadWriteCloser{clientConn},
	}, workspaceMount, api.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9}, client)
	if err == nil {
		t.Fatal("expected stop failure")
	}
	if client.stops != 0 {
		t.Fatalf("stops = %d, want 0", client.stops)
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_mount_stop_failed") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestWorkspaceMaterializerControlledDirtyStopPromotesBeforeFinalize(t *testing.T) {
	captureClient, captureServer := net.Pipe()
	defer captureServer.Close()
	finalClient, finalServer := net.Pipe()
	defer finalServer.Close()
	store := &fakeCAS{objects: map[string][]byte{}}
	body := []byte("dirty workspace")
	object := store.put(workspace.ArtifactMediaType, body)
	workspaceMount := api.WorkerWorkspaceMount{
		ID:                     "mat-1",
		OrgID:                  "org-1",
		ProjectID:              "project-1",
		EnvironmentID:          "environment-1",
		WorkspaceID:            "workspace-1",
		GuestdChannelToken:     "channel-token",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte("channel-token")),
		FencingGeneration:      7,
	}
	done := make(chan error, 2)
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(captureServer); err != nil {
			done <- err
			return
		}
		var request workspacev0.StopWorkspaceRequest
		if err := frameio.ReadProtoFrame(captureServer, &request); err != nil {
			done <- err
			return
		}
		if !request.GetCaptureBeforeStop() || request.GetFinalizeStop() {
			done <- fmt.Errorf("capture stop request = %+v", &request)
			return
		}
		if err := frameio.WriteProtoFrame(captureServer, &workspacev0.StopWorkspaceResponse{
			State: "captured",
			CapturedArtifact: &workspacev0.WorkspaceArtifact{
				Digest:     object.Digest,
				MediaType:  object.MediaType,
				Encoding:   workspace.ArtifactEncoding,
				SizeBytes:  uint64(object.SizeBytes),
				EntryCount: 2,
			},
		}); err != nil {
			done <- err
			return
		}
		entryCount := 2
		if err := wire.WriteStreamFrameHeader(captureServer, wire.StreamHeader{
			Type:        wire.StreamTypeWorkspaceArtifact,
			WorkspaceID: "workspace-1",
			BodyDigest:  &object.Digest,
			EntryCount:  &entryCount,
		}, uint64(len(body))); err != nil {
			done <- err
			return
		}
		_, err := captureServer.Write(body)
		done <- err
	}()
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(finalServer); err != nil {
			done <- err
			return
		}
		var request workspacev0.StopWorkspaceRequest
		if err := frameio.ReadProtoFrame(finalServer, &request); err != nil {
			done <- err
			return
		}
		if request.GetCaptureBeforeStop() || !request.GetFinalizeStop() {
			done <- fmt.Errorf("final stop request = %+v", &request)
			return
		}
		done <- frameio.WriteProtoFrame(finalServer, &workspacev0.StopWorkspaceResponse{State: "stopped"})
	}()
	client := &workspaceMaterializerTestClient{}
	err := (WorkspaceMaterializer{CAS: store}).stopControlledWorkspaceMount(context.Background(), &workspaceMaterializerTestSession{
		streams: []io.ReadWriteCloser{captureClient, finalClient},
	}, workspaceMount, api.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9, DirtyGeneration: 3}, client)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(client.captures) != 1 {
		t.Fatalf("captures = %d, want 1", len(client.captures))
	}
	if client.stops != 1 {
		t.Fatalf("stops = %d, want 1", client.stops)
	}
	if len(client.failures) != 0 {
		t.Fatalf("failures = %+v", client.failures)
	}
}

func TestWorkspaceMaterializerControlledDirtyStopFinalizeFailureFailsWorkspaceMount(t *testing.T) {
	captureClient, captureServer := net.Pipe()
	defer captureServer.Close()
	finalClient, finalServer := net.Pipe()
	_ = finalServer.Close()
	store := &fakeCAS{objects: map[string][]byte{}}
	body := []byte("dirty workspace")
	object := store.put(workspace.ArtifactMediaType, body)
	workspaceMount := api.WorkerWorkspaceMount{
		ID:                     "mat-1",
		OrgID:                  "org-1",
		ProjectID:              "project-1",
		EnvironmentID:          "environment-1",
		WorkspaceID:            "workspace-1",
		GuestdChannelToken:     "channel-token",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte("channel-token")),
		FencingGeneration:      7,
	}
	done := make(chan error, 1)
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(captureServer); err != nil {
			done <- err
			return
		}
		var request workspacev0.StopWorkspaceRequest
		if err := frameio.ReadProtoFrame(captureServer, &request); err != nil {
			done <- err
			return
		}
		if !request.GetCaptureBeforeStop() || request.GetFinalizeStop() {
			done <- fmt.Errorf("capture stop request = %+v", &request)
			return
		}
		if err := frameio.WriteProtoFrame(captureServer, &workspacev0.StopWorkspaceResponse{
			State: "captured",
			CapturedArtifact: &workspacev0.WorkspaceArtifact{
				Digest:     object.Digest,
				MediaType:  object.MediaType,
				Encoding:   workspace.ArtifactEncoding,
				SizeBytes:  uint64(object.SizeBytes),
				EntryCount: 2,
			},
		}); err != nil {
			done <- err
			return
		}
		entryCount := 2
		if err := wire.WriteStreamFrameHeader(captureServer, wire.StreamHeader{
			Type:        wire.StreamTypeWorkspaceArtifact,
			WorkspaceID: "workspace-1",
			BodyDigest:  &object.Digest,
			EntryCount:  &entryCount,
		}, uint64(len(body))); err != nil {
			done <- err
			return
		}
		_, err := captureServer.Write(body)
		done <- err
	}()
	client := &workspaceMaterializerTestClient{}
	err := (WorkspaceMaterializer{CAS: store}).stopControlledWorkspaceMount(context.Background(), &workspaceMaterializerTestSession{
		streams: []io.ReadWriteCloser{captureClient, finalClient},
	}, workspaceMount, api.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9, DirtyGeneration: 3}, client)
	if err == nil {
		t.Fatal("expected finalize failure")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(client.captures) != 1 {
		t.Fatalf("captures = %d, want 1", len(client.captures))
	}
	if client.stops != 0 {
		t.Fatalf("stops = %d, want 0", client.stops)
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_mount_stop_failed") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestWorkspaceMaterializerCleansPartialArtifactsOnMaterializeFailure(t *testing.T) {
	ctx := context.Background()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.WorkspaceArtifact.SizeBytes++
	tempDir := t.TempDir()
	client := &workspaceMaterializerTestClient{}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{},
		CAS:       store,
		TempDir:   tempDir,
	}
	err := materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	if err == nil {
		t.Fatal("expected workspaceMount failure")
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial workspaceMount temp files were not cleaned up: %+v", entries)
	}
}

func TestWorkspaceMaterializerFailsStartupWhenGuestDoesNotRegister(t *testing.T) {
	ctx := context.Background()
	initialClient, initialServer := net.Pipe()
	defer initialServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		_, _, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil {
			return
		}
		var request workspacev0.MaterializeWorkspaceRequest
		if err := frameio.ReadProtoFrame(initialServer, &request); err != nil {
			return
		}
		imageHeader, imageSize, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil || imageHeader.Type != wire.StreamTypeRunImage {
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(imageSize)}); err != nil {
			return
		}
		artifactHeader, artifactSize, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil || artifactHeader.Type != wire.StreamTypeWorkspaceArtifact {
			return
		}
		_, _ = io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(artifactSize)})
		var buf [1]byte
		_, _ = initialServer.Read(buf[:])
	}()
	client := &workspaceMaterializerTestClient{}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{
			initial:   initialClient,
			streams:   []io.ReadWriteCloser{newBlockingReadWriteCloser()},
			operation: discardReadWriteCloser{},
		}},
		CAS:            store,
		TempDir:        t.TempDir(),
		Heartbeat:      time.Hour,
		StartupTimeout: time.Millisecond,
	}
	err := materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("materializer err = %v, want deadline exceeded", err)
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_mount_startup_timeout") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestWorkspaceMaterializerFailsWorkspaceMountOnFatalHeartbeatError(t *testing.T) {
	ctx := context.Background()
	initialClient, initialServer := net.Pipe()
	defer initialServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgeWorkspaceMount(t, initialServer, workspaceMount)
	client := &workspaceMaterializerTestClient{
		renewErrors: []error{errors.New("renew failed")},
	}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{
			initial:   initialClient,
			streams:   []io.ReadWriteCloser{newBlockingReadWriteCloser()},
			operation: discardReadWriteCloser{},
		}},
		CAS:       store,
		TempDir:   t.TempDir(),
		Heartbeat: 10 * time.Millisecond,
		PollEvery: time.Hour,
	}
	err := materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	if err == nil || !strings.Contains(err.Error(), "renew workspace mount") {
		t.Fatalf("materializer err = %v, want renew error", err)
	}
	if len(client.renews) == 0 || client.renews[0].OrgID != "org-1" || client.renews[0].WorkspaceMountID != "mat-1" {
		t.Fatalf("renew requests = %+v", client.renews)
	}
	if len(client.failures) != 1 || client.failures[0].WorkspaceMountID != "mat-1" {
		t.Fatalf("failures = %+v", client.failures)
	}
}

func TestWorkspaceMaterializerFailsWorkspaceMountWhenSessionExits(t *testing.T) {
	ctx := context.Background()
	initialClient, initialServer := net.Pipe()
	defer initialServer.Close()
	exit := make(chan error, 1)
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		acknowledgeWorkspaceMount(t, initialServer, workspaceMount)
		exit <- errors.New("firecracker exited")
	}()
	client := &workspaceMaterializerTestClient{}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{
			initial:   initialClient,
			streams:   []io.ReadWriteCloser{newBlockingReadWriteCloser()},
			operation: discardReadWriteCloser{},
			exit:      exit,
		}},
		CAS:       store,
		TempDir:   t.TempDir(),
		Heartbeat: time.Hour,
		PollEvery: time.Hour,
	}
	err := materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	if err == nil || !strings.Contains(err.Error(), "workspace mount VM exited") {
		t.Fatalf("materializer err = %v, want VM exit", err)
	}
	if len(client.failures) != 1 || client.failures[0].WorkspaceMountID != "mat-1" {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_mount_vm_exited") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestWorkspaceMaterializerRetriesCompletionWithGuestResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialClient, initialServer := net.Pipe()
	eventClient, eventServer := net.Pipe()
	operationClient, operationServer := net.Pipe()
	defer initialServer.Close()
	defer eventServer.Close()
	defer operationServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		_, _, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read materialize header: %v", err)
			return
		}
		var request workspacev0.MaterializeWorkspaceRequest
		if err := frameio.ReadProtoFrame(initialServer, &request); err != nil {
			t.Errorf("read materialize request: %v", err)
			return
		}
		imageHeader, imageSize, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read sandbox image header: %v", err)
			return
		}
		if imageHeader.Type != wire.StreamTypeRunImage {
			t.Errorf("sandbox image header = %+v", imageHeader)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(imageSize)}); err != nil {
			t.Errorf("drain sandbox image: %v", err)
			return
		}
		artifactHeader, artifactSize, err := wire.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read workspace artifact header: %v", err)
			return
		}
		if artifactHeader.Type != wire.StreamTypeWorkspaceArtifact {
			t.Errorf("workspace artifact header = %+v", artifactHeader)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(artifactSize)}); err != nil {
			t.Errorf("drain workspace artifact: %v", err)
			return
		}
		_ = frameio.WriteProtoFrame(initialServer, &workspacev0.MaterializeWorkspaceResponse{
			State:                  "running",
			GuestdChannelTokenHash: sha256sum.HexBytes([]byte("channel-token")),
		})
	}()
	go func() {
		_, _, err := wire.ReadStreamFrameHeader(eventServer)
		if err != nil {
			t.Errorf("read event header: %v", err)
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := frameio.ReadProtoFrame(eventServer, &envelope); err != nil {
			t.Errorf("read event envelope: %v", err)
			return
		}
		<-ctx.Done()
	}()
	go func() {
		_, _, err := wire.ReadStreamFrameHeader(operationServer)
		if err != nil {
			t.Errorf("read operation header: %v", err)
			return
		}
		var request workspacev0.WorkspaceOperationRequest
		if err := frameio.ReadProtoFrame(operationServer, &request); err != nil {
			t.Errorf("read operation request: %v", err)
			return
		}
		_ = frameio.WriteProtoFrame(operationServer, &workspacev0.WorkspaceOperationResult{ResultJson: `{"ok":true}`})
	}()
	client := &workspaceMaterializerTestClient{
		cancel:           cancel,
		completionErrors: []error{errors.New("transient completion failure")},
		operation: &api.WorkerWorkspaceOperation{
			WorkspaceOperationResponse: api.WorkspaceOperationResponse{
				ID:                 "operation-1",
				OrgID:              "org-1",
				WorkspaceID:        "workspace-1",
				WorkspaceMountID:   "mat-1",
				OperationKind:      "StartExec",
				FencingGeneration:  1,
				RequestFingerprint: testWorkspaceOperationFingerprint("StartExec", `{"exec_id":"exec-1","command":["echo","ok"],"detached":false}`),
				OperationExpiresAt: time.Now().Add(time.Hour),
				Request:            []byte(`{"exec_id":"exec-1","command":["echo","ok"],"detached":false}`),
			},
			ClaimToken: "claim-token",
		},
	}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{
			initial: initialClient,
			streams: []io.ReadWriteCloser{eventClient, operationClient},
		}},
		CAS:                  store,
		TempDir:              t.TempDir(),
		Heartbeat:            time.Hour,
		PollEvery:            time.Millisecond,
		CompleteErrorBackoff: time.Millisecond,
	}
	err := materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("materializer err = %v, want context canceled", err)
	}
	if len(client.completions) != 2 {
		t.Fatalf("completion attempts = %d", len(client.completions))
	}
	for _, completion := range client.completions {
		if string(completion.Result) != `{"ok":true}` || len(completion.Error) != 0 {
			t.Fatalf("completion retry changed guest result: %+v", completion)
		}
	}
}

func TestWorkspaceMaterializerPersistsWorkspaceOperationEvents(t *testing.T) {
	materializer := WorkspaceMaterializer{}
	client := &workspaceMaterializerTestClient{}
	workspaceMount := api.WorkerWorkspaceMount{
		ID:            "mat-1",
		OrgID:         "org-1",
		ProjectID:     "project-1",
		EnvironmentID: "env-1",
		WorkspaceID:   "workspace-1",
	}
	events := []*workspacev0.WorkspaceOperationEvent{
		{Event: &workspacev0.WorkspaceOperationEvent_ExecStarted{ExecStarted: &workspacev0.WorkspaceExecStarted{ExecId: "exec-1", ProcessId: "pid-1"}}},
		{Event: &workspacev0.WorkspaceOperationEvent_ExecStdoutChunk{ExecStdoutChunk: &workspacev0.WorkspaceExecOutputChunk{ExecId: "exec-1", Data: []byte("hello")}}},
		{Event: &workspacev0.WorkspaceOperationEvent_ExecStdoutChunk{ExecStdoutChunk: &workspacev0.WorkspaceExecOutputChunk{ExecId: "exec-1", Data: []byte(" world")}}},
		{Event: &workspacev0.WorkspaceOperationEvent_ExecExited{ExecExited: &workspacev0.WorkspaceExecExited{ExecId: "exec-1", ExitCode: 7}}},
		{Event: &workspacev0.WorkspaceOperationEvent_PtyOpened{PtyOpened: &workspacev0.WorkspacePtyOpened{PtyId: "pty-1", ProcessId: "pid-2", Cols: 80, Rows: 24}}},
		{Event: &workspacev0.WorkspaceOperationEvent_PtyOutputChunk{PtyOutputChunk: &workspacev0.WorkspacePtyOutputChunk{PtyId: "pty-1", Data: []byte("pty")}}},
		{Event: &workspacev0.WorkspaceOperationEvent_PtyResizeApplied{PtyResizeApplied: &workspacev0.WorkspacePtyResizeApplied{PtyId: "pty-1", Cols: 100, Rows: 30}}},
		{Event: &workspacev0.WorkspaceOperationEvent_PtyClosed{PtyClosed: &workspacev0.WorkspacePtyClosed{PtyId: "pty-1", Reason: "exit"}}},
	}
	persistState := newWorkspaceOperationEventPersistState()
	for _, event := range events {
		if err := materializer.persistWorkspaceOperationEvent(context.Background(), client, workspaceMount, persistState, event); err != nil {
			t.Fatalf("persist event %T: %v", event.GetEvent(), err)
		}
	}
	if len(client.execStarted) != 1 || client.execStarted[0].ExecID != "exec-1" || client.execStarted[0].ProcessID != "pid-1" {
		t.Fatalf("exec started = %+v", client.execStarted)
	}
	if len(client.execOutput) != 2 || client.execOutput[0].Chunks[0].Stream != "stdout" || client.execOutput[0].Chunks[0].OffsetStart == nil || *client.execOutput[0].Chunks[0].OffsetStart != 0 || string(client.execOutput[0].Chunks[0].Data) != "hello" || client.execOutput[1].Chunks[0].OffsetStart == nil || *client.execOutput[1].Chunks[0].OffsetStart != 5 || string(client.execOutput[1].Chunks[0].Data) != " world" {
		t.Fatalf("exec output = %+v", client.execOutput)
	}
	if len(client.execExited) != 1 || client.execExited[0].State != "exited" || client.execExited[0].ExitCode == nil || *client.execExited[0].ExitCode != 7 {
		t.Fatalf("exec exited = %+v", client.execExited)
	}
	if len(client.ptyOpened) != 1 || client.ptyOpened[0].PtyID != "pty-1" || client.ptyOpened[0].ProcessID != "pid-2" {
		t.Fatalf("pty opened = %+v", client.ptyOpened)
	}
	if len(client.ptyOutput) != 1 || client.ptyOutput[0].Chunks[0].OffsetStart == nil || *client.ptyOutput[0].Chunks[0].OffsetStart != 0 || string(client.ptyOutput[0].Chunks[0].Data) != "pty" {
		t.Fatalf("pty output = %+v", client.ptyOutput)
	}
	if len(client.ptyResizeApplied) != 1 || client.ptyResizeApplied[0].Cols != 100 || client.ptyResizeApplied[0].Rows != 30 {
		t.Fatalf("pty resize applied = %+v", client.ptyResizeApplied)
	}
	if len(client.ptyClosed) != 1 || client.ptyClosed[0].Reason != "exit" {
		t.Fatalf("pty closed = %+v", client.ptyClosed)
	}
}

func TestWorkspaceMaterializerRetriesOutputEventWithStableOffset(t *testing.T) {
	materializer := WorkspaceMaterializer{CompleteErrorBackoff: time.Millisecond}
	client := &workspaceMaterializerTestClient{
		execOutputErrors: []error{errors.New("transient response loss")},
	}
	workspaceMount := api.WorkerWorkspaceMount{
		ID:            "mat-1",
		OrgID:         "org-1",
		ProjectID:     "project-1",
		EnvironmentID: "env-1",
		WorkspaceID:   "workspace-1",
	}
	event := &workspacev0.WorkspaceOperationEvent{
		Event: &workspacev0.WorkspaceOperationEvent_ExecStdoutChunk{
			ExecStdoutChunk: &workspacev0.WorkspaceExecOutputChunk{ExecId: "exec-1", Data: []byte("hello")},
		},
	}
	state := newWorkspaceOperationEventPersistState()
	err := retryWorkspaceOperation(context.Background(), materializer.CompleteErrorBackoff, func() error {
		return materializer.persistWorkspaceOperationEvent(context.Background(), client, workspaceMount, state, event)
	})
	if err != nil {
		t.Fatalf("retry output event: %v", err)
	}
	if len(client.execOutput) != 2 {
		t.Fatalf("exec output attempts = %d, want 2", len(client.execOutput))
	}
	for _, request := range client.execOutput {
		if request.Chunks[0].OffsetStart == nil || *request.Chunks[0].OffsetStart != 0 {
			t.Fatalf("retry used unstable offset: %+v", client.execOutput)
		}
	}
	if got := state.execOutputOffset("exec-1", "stdout"); got != 5 {
		t.Fatalf("next output offset = %d, want 5", got)
	}
}

func TestWorkspaceMaterializerRegistersPreparedRuntimeOverOpenedStream(t *testing.T) {
	ctx := context.Background()
	initialClient, initialServer := net.Pipe()
	preparedClient, preparedServer := net.Pipe()
	defer initialClient.Close()
	defer initialServer.Close()
	defer preparedServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	workspacePath := filepath.Join(t.TempDir(), "workspace.tar")
	if err := os.WriteFile(workspacePath, store.objects[workspaceMount.WorkspaceArtifact.Digest], 0o600); err != nil {
		t.Fatal(err)
	}
	session := &workspaceMaterializerTestSession{
		initial: initialClient,
		streams: []io.ReadWriteCloser{preparedClient},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		acknowledgePreparedWorkspaceMount(t, preparedServer, workspaceMount, "runtime-key")
	}()

	err := (WorkspaceMaterializer{}).registerWorkspaceMount(ctx, session, workspaceMount, "", workspacePath, "runtime-key", true)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if len(session.opened) != 1 || session.opened[0] != preparedClient {
		t.Fatalf("opened streams = %+v, want prepared runtime workspaceMount over OpenStream", session.opened)
	}
}

func acknowledgeWorkspaceMount(t *testing.T, stream io.ReadWriteCloser, workspaceMount api.WorkerWorkspaceMount) {
	t.Helper()
	_, _, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read materialize header: %v", err)
		return
	}
	var request workspacev0.MaterializeWorkspaceRequest
	if err := frameio.ReadProtoFrame(stream, &request); err != nil {
		t.Errorf("read materialize request: %v", err)
		return
	}
	imageHeader, imageSize, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read sandbox image header: %v", err)
		return
	}
	if imageHeader.Type != wire.StreamTypeRunImage {
		t.Errorf("sandbox image header = %+v", imageHeader)
		return
	}
	if _, err := io.Copy(io.Discard, &io.LimitedReader{R: stream, N: int64(imageSize)}); err != nil {
		t.Errorf("drain sandbox image: %v", err)
		return
	}
	artifactHeader, artifactSize, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read workspace artifact header: %v", err)
		return
	}
	if artifactHeader.Type != wire.StreamTypeWorkspaceArtifact {
		t.Errorf("workspace artifact header = %+v", artifactHeader)
		return
	}
	if _, err := io.Copy(io.Discard, &io.LimitedReader{R: stream, N: int64(artifactSize)}); err != nil {
		t.Errorf("drain workspace artifact: %v", err)
		return
	}
	_ = frameio.WriteProtoFrame(stream, &workspacev0.MaterializeWorkspaceResponse{
		State:                  "running",
		GuestdChannelTokenHash: workspaceMount.GuestdChannelTokenHash,
	})
}

func acknowledgePreparedWorkspaceMount(t *testing.T, stream io.ReadWriteCloser, workspaceMount api.WorkerWorkspaceMount, runtimeKey string) {
	t.Helper()
	header, _, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read materialize header: %v", err)
		return
	}
	if header.Type != wire.StreamTypeWorkspaceMaterialize {
		t.Errorf("materialize header = %+v", header)
		return
	}
	var request workspacev0.MaterializeWorkspaceRequest
	if err := frameio.ReadProtoFrame(stream, &request); err != nil {
		t.Errorf("read materialize request: %v", err)
		return
	}
	if !request.UsePreparedRuntime || request.RuntimeKey != runtimeKey {
		t.Errorf("prepared runtime request use=%v key=%q", request.UsePreparedRuntime, request.RuntimeKey)
		return
	}
	artifactHeader, artifactSize, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read workspace artifact header: %v", err)
		return
	}
	if artifactHeader.Type != wire.StreamTypeWorkspaceArtifact {
		t.Errorf("workspace artifact header = %+v", artifactHeader)
		return
	}
	if _, err := io.Copy(io.Discard, &io.LimitedReader{R: stream, N: int64(artifactSize)}); err != nil {
		t.Errorf("drain workspace artifact: %v", err)
		return
	}
	_ = frameio.WriteProtoFrame(stream, &workspacev0.MaterializeWorkspaceResponse{
		State:                  "running",
		GuestdChannelTokenHash: workspaceMount.GuestdChannelTokenHash,
	})
}

type workspaceMaterializerTestConnector struct {
	session  vm.Session
	requests *[]vm.MaterializeRequest
}

func (c workspaceMaterializerTestConnector) Connect(context.Context, vm.ConnectRequest) (vm.Session, error) {
	return c.session, nil
}

func (c workspaceMaterializerTestConnector) Materialize(_ context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	if request.RootfsDigest == "" || request.ImageDigest == "" || request.ImageFormat != "oci-tar" || request.BaseVersionID == "" {
		return nil, errors.New("materialize request missing runtime authority")
	}
	if c.requests != nil {
		*c.requests = append(*c.requests, request)
	}
	return c.session, nil
}

type parallelStartGate struct {
	mu      sync.Mutex
	seen    map[string]bool
	started chan string
	release chan struct{}
}

func newParallelStartGate() *parallelStartGate {
	return &parallelStartGate{
		seen:    map[string]bool{},
		started: make(chan string, 3),
		release: make(chan struct{}),
	}
}

func (g *parallelStartGate) wait(ctx context.Context, label string) error {
	g.mu.Lock()
	if !g.seen[label] {
		g.seen[label] = true
		g.started <- label
	}
	g.mu.Unlock()
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type parallelStartCAS struct {
	cas.Store
	gate           *parallelStartGate
	workspaceMount api.WorkerWorkspaceMount
}

func (c parallelStartCAS) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	label := "unknown-artifact"
	switch strings.TrimSpace(digest) {
	case strings.TrimSpace(c.workspaceMount.SandboxImageArtifact.Digest):
		label = "sandbox-image"
	case strings.TrimSpace(c.workspaceMount.WorkspaceArtifact.Digest):
		label = "workspace-image"
	}
	if err := c.gate.wait(ctx, label); err != nil {
		return nil, err
	}
	return c.Store.Get(ctx, digest)
}

type parallelStartConnector struct {
	gate    *parallelStartGate
	session vm.Session
}

func (c parallelStartConnector) Connect(context.Context, vm.ConnectRequest) (vm.Session, error) {
	return c.session, nil
}

func (c parallelStartConnector) Materialize(ctx context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	if request.RootfsDigest == "" || request.ImageDigest == "" || request.ImageFormat != "oci-tar" || request.BaseVersionID == "" {
		return nil, errors.New("materialize request missing runtime authority")
	}
	if err := c.gate.wait(ctx, "connector"); err != nil {
		return nil, err
	}
	return c.session, nil
}

type workspaceMaterializerTestSession struct {
	initial   io.ReadWriteCloser
	operation io.ReadWriteCloser
	streams   []io.ReadWriteCloser
	opened    []io.ReadWriteCloser
	exit      <-chan error
	closeErr  error
	closed    int
}

func (s *workspaceMaterializerTestSession) Stream() io.ReadWriteCloser {
	return s.initial
}

func (s *workspaceMaterializerTestSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	if len(s.streams) > 0 {
		stream := s.streams[0]
		s.streams = s.streams[1:]
		s.opened = append(s.opened, stream)
		return stream, nil
	}
	s.opened = append(s.opened, s.operation)
	return s.operation, nil
}

func (s *workspaceMaterializerTestSession) Close(context.Context) error {
	s.closed++
	if s.initial != nil {
		_ = s.initial.Close()
	}
	if s.operation != nil {
		_ = s.operation.Close()
	}
	for _, stream := range s.opened {
		if stream != nil {
			_ = stream.Close()
		}
	}
	for _, stream := range s.streams {
		_ = stream.Close()
	}
	return s.closeErr
}

func (s *workspaceMaterializerTestSession) Wait(ctx context.Context) error {
	if s.exit == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case err := <-s.exit:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type workspaceMaterializerTestClient struct {
	cancel           context.CancelFunc
	operation        *api.WorkerWorkspaceOperation
	completed        api.WorkerWorkspaceOperationCompleteRequest
	completions      []api.WorkerWorkspaceOperationCompleteRequest
	completionErrors []error
	renewErrors      []error
	renews           []api.WorkerWorkspaceMountRenewRequest
	mounted          []api.WorkerWorkspaceMountMountedRequest
	claims           []api.WorkerWorkspaceOperationClaimRequest
	starts           []api.WorkerWorkspaceOperationStartRequest
	startErrors      []error
	stops            int
	captures         []api.WorkerWorkspaceMountCaptureRequest
	failures         []api.WorkerWorkspaceMountFailRequest
	execStarted      []api.WorkerWorkspaceExecStartedRequest
	execOutput       []api.WorkerWorkspaceExecOutputRequest
	execOutputErrors []error
	execInput        []api.WorkerWorkspaceExecInputResponse
	execDelivered    []api.WorkerWorkspaceExecInputDeliveredRequest
	execExited       []api.WorkerWorkspaceExecExitedRequest
	ptyOpened        []api.WorkerWorkspacePtyOpenedRequest
	ptyOutput        []api.WorkerWorkspacePtyOutputRequest
	ptyOutputErrors  []error
	ptyInput         []api.WorkerWorkspacePtyInputResponse
	ptyDelivered     []api.WorkerWorkspacePtyInputDeliveredRequest
	ptyResizeApplied []api.WorkerWorkspacePtyResizeAppliedRequest
	ptyClosed        []api.WorkerWorkspacePtyClosedRequest
}

func (c *workspaceMaterializerTestClient) RenewWorkspaceMount(_ context.Context, request api.WorkerWorkspaceMountRenewRequest) (api.WorkspaceMountResponse, error) {
	c.renews = append(c.renews, request)
	if len(c.renewErrors) > 0 {
		err := c.renewErrors[0]
		c.renewErrors = c.renewErrors[1:]
		return api.WorkspaceMountResponse{}, err
	}
	return api.WorkspaceMountResponse{State: "mounting"}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspaceMountMounted(_ context.Context, request api.WorkerWorkspaceMountMountedRequest) (api.WorkspaceMountResponse, error) {
	c.mounted = append(c.mounted, request)
	return api.WorkspaceMountResponse{State: "mounted"}, nil
}

func (c *workspaceMaterializerTestClient) CaptureWorkspaceMount(_ context.Context, request api.WorkerWorkspaceMountCaptureRequest) (api.WorkerWorkspaceMountCaptureResponse, error) {
	c.captures = append(c.captures, request)
	return api.WorkerWorkspaceMountCaptureResponse{VersionID: "version-1"}, nil
}

func (c *workspaceMaterializerTestClient) StopWorkspaceMount(context.Context, api.WorkerWorkspaceMountStopRequest) (api.WorkspaceMountResponse, error) {
	c.stops++
	return api.WorkspaceMountResponse{State: "unmounted"}, nil
}

func (c *workspaceMaterializerTestClient) FailWorkspaceMount(_ context.Context, request api.WorkerWorkspaceMountFailRequest) (api.WorkspaceMountResponse, error) {
	c.failures = append(c.failures, request)
	return api.WorkspaceMountResponse{State: "failed"}, nil
}

func (c *workspaceMaterializerTestClient) ClaimWorkspaceOperation(_ context.Context, request api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error) {
	c.claims = append(c.claims, request)
	if c.operation == nil {
		return api.WorkerWorkspaceOperationClaimResponse{}, nil
	}
	operation := c.operation
	c.operation = nil
	return api.WorkerWorkspaceOperationClaimResponse{Operation: operation}, nil
}

func (c *workspaceMaterializerTestClient) StartWorkspaceOperation(_ context.Context, request api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error) {
	c.starts = append(c.starts, request)
	if len(c.startErrors) > 0 {
		err := c.startErrors[0]
		c.startErrors = c.startErrors[1:]
		return api.WorkspaceOperationResponse{}, err
	}
	return api.WorkspaceOperationResponse{ID: request.OperationID, State: "running"}, nil
}

func (c *workspaceMaterializerTestClient) CompleteWorkspaceOperation(_ context.Context, request api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error) {
	c.completed = request
	c.completions = append(c.completions, request)
	if len(c.completionErrors) > 0 {
		err := c.completionErrors[0]
		c.completionErrors = c.completionErrors[1:]
		return api.WorkspaceOperationResponse{}, err
	}
	if c.cancel != nil {
		c.cancel()
	}
	return api.WorkspaceOperationResponse{ID: request.OperationID, State: "completed"}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspaceExecStarted(_ context.Context, request api.WorkerWorkspaceExecStartedRequest) (api.WorkspaceExecEnvelope, error) {
	c.execStarted = append(c.execStarted, request)
	return api.WorkspaceExecEnvelope{}, nil
}

func (c *workspaceMaterializerTestClient) AppendWorkspaceExecOutput(_ context.Context, request api.WorkerWorkspaceExecOutputRequest) (api.ListWorkspaceExecStreamChunksResponse, error) {
	c.execOutput = append(c.execOutput, request)
	if len(c.execOutputErrors) > 0 {
		err := c.execOutputErrors[0]
		c.execOutputErrors = c.execOutputErrors[1:]
		return api.ListWorkspaceExecStreamChunksResponse{}, err
	}
	out := make([]api.WorkspaceExecStreamChunkResponse, 0, len(request.Chunks))
	for _, chunk := range request.Chunks {
		offset := int64(0)
		if chunk.OffsetStart != nil {
			offset = *chunk.OffsetStart
		}
		out = append(out, api.WorkspaceExecStreamChunkResponse{
			Stream:      chunk.Stream,
			OffsetStart: offset,
			OffsetEnd:   offset + int64(len(chunk.Data)),
			Data:        chunk.Data,
		})
	}
	return api.ListWorkspaceExecStreamChunksResponse{Chunks: out}, nil
}

func (c *workspaceMaterializerTestClient) ListWorkspaceExecInput(context.Context, api.WorkerWorkspaceExecInputRequest) (api.WorkerWorkspaceExecInputResponse, error) {
	if len(c.execInput) > 0 {
		response := c.execInput[0]
		c.execInput = c.execInput[1:]
		return response, nil
	}
	return api.WorkerWorkspaceExecInputResponse{}, nil
}

func (c *workspaceMaterializerTestClient) AdvanceWorkspaceExecInputDelivered(_ context.Context, request api.WorkerWorkspaceExecInputDeliveredRequest) (api.WorkspaceExecEnvelope, error) {
	c.execDelivered = append(c.execDelivered, request)
	return api.WorkspaceExecEnvelope{}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspaceExecExited(_ context.Context, request api.WorkerWorkspaceExecExitedRequest) (api.WorkspaceExecEnvelope, error) {
	c.execExited = append(c.execExited, request)
	return api.WorkspaceExecEnvelope{}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspacePtyOpened(_ context.Context, request api.WorkerWorkspacePtyOpenedRequest) (api.WorkspacePtyEnvelope, error) {
	c.ptyOpened = append(c.ptyOpened, request)
	return api.WorkspacePtyEnvelope{}, nil
}

func (c *workspaceMaterializerTestClient) AppendWorkspacePtyOutput(_ context.Context, request api.WorkerWorkspacePtyOutputRequest) (api.ListWorkspacePtyStreamChunksResponse, error) {
	c.ptyOutput = append(c.ptyOutput, request)
	if len(c.ptyOutputErrors) > 0 {
		err := c.ptyOutputErrors[0]
		c.ptyOutputErrors = c.ptyOutputErrors[1:]
		return api.ListWorkspacePtyStreamChunksResponse{}, err
	}
	out := make([]api.WorkspacePtyStreamChunkResponse, 0, len(request.Chunks))
	for _, chunk := range request.Chunks {
		offset := int64(0)
		if chunk.OffsetStart != nil {
			offset = *chunk.OffsetStart
		}
		out = append(out, api.WorkspacePtyStreamChunkResponse{
			Stream:      "output",
			OffsetStart: offset,
			OffsetEnd:   offset + int64(len(chunk.Data)),
			Data:        chunk.Data,
		})
	}
	return api.ListWorkspacePtyStreamChunksResponse{Chunks: out}, nil
}

func (c *workspaceMaterializerTestClient) ListWorkspacePtyInput(context.Context, api.WorkerWorkspacePtyInputRequest) (api.WorkerWorkspacePtyInputResponse, error) {
	if len(c.ptyInput) > 0 {
		response := c.ptyInput[0]
		c.ptyInput = c.ptyInput[1:]
		return response, nil
	}
	return api.WorkerWorkspacePtyInputResponse{}, nil
}

func (c *workspaceMaterializerTestClient) AdvanceWorkspacePtyInputDelivered(_ context.Context, request api.WorkerWorkspacePtyInputDeliveredRequest) (api.WorkspacePtyEnvelope, error) {
	c.ptyDelivered = append(c.ptyDelivered, request)
	return api.WorkspacePtyEnvelope{}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspacePtyResizeApplied(_ context.Context, request api.WorkerWorkspacePtyResizeAppliedRequest) (api.WorkspacePtyEnvelope, error) {
	c.ptyResizeApplied = append(c.ptyResizeApplied, request)
	return api.WorkspacePtyEnvelope{}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspacePtyClosed(_ context.Context, request api.WorkerWorkspacePtyClosedRequest) (api.WorkspacePtyEnvelope, error) {
	c.ptyClosed = append(c.ptyClosed, request)
	return api.WorkspacePtyEnvelope{}, nil
}

func testWorkspaceOperationFingerprint(operationKind string, requestJSON string) string {
	fingerprint, err := wire.RequestFingerprint(operationKind, []byte(requestJSON))
	if err != nil {
		panic(err)
	}
	return fingerprint
}

func TestValidateWorkspaceInputAckRequiresExpectedScopeAndOffset(t *testing.T) {
	ack := &workspacev0.WorkspaceStreamAck{
		ResourceKind:  "workspace_exec",
		ResourceId:    "exec-1",
		Stream:        "stdin",
		DurableOffset: 5,
	}
	if err := validateWorkspaceInputAck(ack, "workspace_exec", "exec-1", "stdin", 5); err != nil {
		t.Fatalf("valid ack rejected: %v", err)
	}
	if err := validateWorkspaceInputAck(ack, "workspace_exec", "exec-1", "stdin", 4); err == nil {
		t.Fatal("offset mismatch accepted")
	}
	if err := validateWorkspaceInputAck(ack, "workspace_pty", "exec-1", "stdin", 5); err == nil {
		t.Fatal("resource kind mismatch accepted")
	}
	if err := validateWorkspaceInputAck(ack, "workspace_exec", "exec-2", "stdin", 5); err == nil {
		t.Fatal("resource id mismatch accepted")
	}
	if err := validateWorkspaceInputAck(ack, "workspace_exec", "exec-1", "input", 5); err == nil {
		t.Fatal("stream mismatch accepted")
	}
}

func TestWorkspaceExecInputRelayClosesAfterAllPagedInputIsDelivered(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	chunks := make([]api.WorkspaceExecStreamChunkResponse, 101)
	for i := range chunks {
		chunks[i] = api.WorkspaceExecStreamChunkResponse{
			Stream:      "stdin",
			OffsetStart: int64(i),
			OffsetEnd:   int64(i + 1),
			Data:        []byte("x"),
		}
	}
	closedAt := time.Now()
	control := &workspaceMaterializerTestClient{execInput: []api.WorkerWorkspaceExecInputResponse{
		{Chunks: chunks[:100], StdinClosedAt: &closedAt, StdinCursor: 101, StdinDeliveredCursor: 0, State: "running"},
		{Chunks: chunks[100:], StdinClosedAt: &closedAt, StdinCursor: 101, StdinDeliveredCursor: 100, State: "running"},
		{Chunks: nil, StdinClosedAt: &closedAt, StdinCursor: 101, StdinDeliveredCursor: 101, State: "exited"},
	}}
	guestErr := make(chan error, 1)
	go func() {
		_, bodyLen, err := wire.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := frameio.ReadProtoFrame(guestStream, &envelope); err != nil {
			guestErr <- err
			return
		}
		if envelope.GetFencingGeneration() != 2 || envelope.GetWriteLeaseId() != "write-lease-1" || envelope.GetFencingToken() != "write-token-1" {
			guestErr <- fmt.Errorf("input envelope generation=%d write_lease=%q fencing_token=%q", envelope.GetFencingGeneration(), envelope.GetWriteLeaseId(), envelope.GetFencingToken())
			return
		}
		for expected := range uint64(101) {
			var frame workspacev0.WorkspaceInputFrame
			if err := frameio.ReadProtoFrame(guestStream, &frame); err != nil {
				guestErr <- err
				return
			}
			chunk := frame.GetChunk()
			if chunk == nil {
				guestErr <- errors.New("received input close before all chunks")
				return
			}
			if chunk.GetOffsetStart() != expected {
				guestErr <- fmt.Errorf("chunk offset = %d, want %d", chunk.GetOffsetStart(), expected)
				return
			}
			if err := frameio.WriteProtoFrame(guestStream, &workspacev0.WorkspaceStreamAck{
				ResourceKind:  chunk.GetResourceKind(),
				ResourceId:    chunk.GetResourceId(),
				Stream:        chunk.GetStream(),
				DurableOffset: expected + 1,
			}); err != nil {
				guestErr <- err
				return
			}
		}
		var frame workspacev0.WorkspaceInputFrame
		if err := frameio.ReadProtoFrame(guestStream, &frame); err != nil {
			guestErr <- err
			return
		}
		closeFrame := frame.GetClose()
		if closeFrame == nil {
			guestErr <- errors.New("missing input close after all chunks")
			return
		}
		if closeFrame.GetOffset() != 101 {
			guestErr <- fmt.Errorf("close offset = %d, want 101", closeFrame.GetOffset())
			return
		}
		if err := frameio.WriteProtoFrame(guestStream, &workspacev0.WorkspaceStreamAck{
			ResourceKind:  closeFrame.GetResourceKind(),
			ResourceId:    closeFrame.GetResourceId(),
			Stream:        closeFrame.GetStream(),
			DurableOffset: closeFrame.GetOffset(),
		}); err != nil {
			guestErr <- err
			return
		}
		guestErr <- nil
	}()
	session := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{clientStream}}
	workspaceMount := api.WorkerWorkspaceMount{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		WorkspaceMountID:   "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "exec-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspaceExecInputRelay(context.Background(), session, workspaceMount, operation, "exec-1", control); err != nil {
		t.Fatalf("run input relay: %v", err)
	}
	if err := <-guestErr; err != nil {
		t.Fatalf("guest stream: %v", err)
	}
	if len(control.execDelivered) != 101 {
		t.Fatalf("delivered calls = %d, want 101", len(control.execDelivered))
	}
}

func TestWorkspaceExecInputRelayUsesPersistedDeliveredCursorForClose(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	closedAt := time.Now()
	control := &workspaceMaterializerTestClient{execInput: []api.WorkerWorkspaceExecInputResponse{
		{Chunks: nil, StdinClosedAt: &closedAt, StdinCursor: 101, StdinDeliveredCursor: 101, State: "running"},
		{Chunks: nil, StdinClosedAt: &closedAt, StdinCursor: 101, StdinDeliveredCursor: 101, State: "exited"},
	}}
	guestErr := make(chan error, 1)
	go func() {
		_, bodyLen, err := wire.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := frameio.ReadProtoFrame(guestStream, &envelope); err != nil {
			guestErr <- err
			return
		}
		var frame workspacev0.WorkspaceInputFrame
		if err := frameio.ReadProtoFrame(guestStream, &frame); err != nil {
			guestErr <- err
			return
		}
		closeFrame := frame.GetClose()
		if closeFrame == nil {
			guestErr <- errors.New("expected close frame")
			return
		}
		if closeFrame.GetOffset() != 101 {
			guestErr <- fmt.Errorf("close offset = %d, want 101", closeFrame.GetOffset())
			return
		}
		guestErr <- frameio.WriteProtoFrame(guestStream, &workspacev0.WorkspaceStreamAck{
			ResourceKind:  closeFrame.GetResourceKind(),
			ResourceId:    closeFrame.GetResourceId(),
			Stream:        closeFrame.GetStream(),
			DurableOffset: closeFrame.GetOffset(),
		})
	}()
	session := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{clientStream}}
	workspaceMount := api.WorkerWorkspaceMount{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		WorkspaceMountID:   "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "exec-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspaceExecInputRelay(context.Background(), session, workspaceMount, operation, "exec-1", control); err != nil {
		t.Fatalf("run input relay: %v", err)
	}
	if err := <-guestErr; err != nil {
		t.Fatalf("guest stream: %v", err)
	}
}

func TestWorkspaceExecInputRelayStopsBeforeDeliveringTerminalInput(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	control := &workspaceMaterializerTestClient{execInput: []api.WorkerWorkspaceExecInputResponse{
		{
			Chunks: []api.WorkspaceExecStreamChunkResponse{{
				Stream:      "stdin",
				OffsetStart: 0,
				OffsetEnd:   1,
				Data:        []byte("x"),
			}},
			StdinCursor:          1,
			StdinDeliveredCursor: 0,
			State:                "exited",
		},
	}}
	guestErr := make(chan error, 1)
	go func() {
		_, bodyLen, err := wire.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := frameio.ReadProtoFrame(guestStream, &envelope); err != nil {
			guestErr <- err
			return
		}
		_, err = guestStream.Read(make([]byte, 1))
		if !errors.Is(err, io.EOF) {
			guestErr <- fmt.Errorf("terminal relay wrote input frame or did not close stream: %v", err)
			return
		}
		guestErr <- nil
	}()
	session := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{clientStream}}
	workspaceMount := api.WorkerWorkspaceMount{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		WorkspaceMountID:   "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "exec-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspaceExecInputRelay(context.Background(), session, workspaceMount, operation, "exec-1", control); err != nil {
		t.Fatalf("run input relay: %v", err)
	}
	if err := <-guestErr; err != nil {
		t.Fatalf("guest stream: %v", err)
	}
	if len(control.execDelivered) != 0 {
		t.Fatalf("delivered calls = %d, want 0", len(control.execDelivered))
	}
}

func TestWorkspacePtyInputRelayStopsBeforeDeliveringTerminalInput(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	control := &workspaceMaterializerTestClient{ptyInput: []api.WorkerWorkspacePtyInputResponse{
		{
			Chunks: []api.WorkspacePtyStreamChunkResponse{{
				Stream:      "input",
				OffsetStart: 0,
				OffsetEnd:   1,
				Data:        []byte("x"),
			}},
			InputCursor: 1,
			State:       "closed",
		},
	}}
	guestErr := make(chan error, 1)
	go func() {
		_, bodyLen, err := wire.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := frameio.ReadProtoFrame(guestStream, &envelope); err != nil {
			guestErr <- err
			return
		}
		_, err = guestStream.Read(make([]byte, 1))
		if !errors.Is(err, io.EOF) {
			guestErr <- fmt.Errorf("terminal pty relay wrote input frame or did not close stream: %v", err)
			return
		}
		guestErr <- nil
	}()
	session := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{clientStream}}
	workspaceMount := api.WorkerWorkspaceMount{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		WorkspaceMountID:   "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "pty-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspacePtyInputRelay(context.Background(), session, workspaceMount, operation, "pty-1", control); err != nil {
		t.Fatalf("run pty input relay: %v", err)
	}
	if err := <-guestErr; err != nil {
		t.Fatalf("guest stream: %v", err)
	}
	if len(control.ptyDelivered) != 0 {
		t.Fatalf("pty delivered calls = %d, want 0", len(control.ptyDelivered))
	}
}

func TestReportWorkspaceInputRelayFailureEmitsWorkspaceMountFailure(t *testing.T) {
	failures := make(chan error, 1)
	WorkspaceMaterializer{}.reportWorkspaceInputRelayFailure(context.Background(), failures, "exec", "exec-1", func() error {
		return errors.New("ack failed")
	})
	select {
	case err := <-failures:
		var failure workspaceMountFailure
		if !errors.As(err, &failure) {
			t.Fatalf("failure type = %T, want workspaceMountFailure", err)
		}
		if failure.code != "workspace_mount_input_stream_lost" {
			t.Fatalf("failure code = %q", failure.code)
		}
		if !strings.Contains(err.Error(), "exec-1") || !strings.Contains(err.Error(), "ack failed") {
			t.Fatalf("failure error = %v, want resource and cause", err)
		}
	default:
		t.Fatal("relay failure was not reported")
	}
}

func TestReportWorkspaceInputRelayFailureDoesNotDropBehindPendingFailure(t *testing.T) {
	failures := make(chan error, 1)
	failures <- workspaceMountFailure{code: "pending", err: errors.New("pending")}
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		WorkspaceMaterializer{}.reportWorkspaceInputRelayFailure(context.Background(), failures, "exec", "exec-1", func() error {
			return errors.New("ack failed")
		})
	}()

	select {
	case <-reported:
		t.Fatal("relay failure returned before the pending failure was consumed")
	default:
	}
	<-failures
	select {
	case err := <-failures:
		if !strings.Contains(err.Error(), "ack failed") {
			t.Fatalf("failure error = %v, want relay cause", err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay failure was dropped behind pending failure")
	}
	select {
	case <-reported:
	case <-time.After(time.Second):
		t.Fatal("relay failure reporter did not return after delivering failure")
	}
}

func TestWorkspaceInputRelayRegistryDeduplicatesLiveRelay(t *testing.T) {
	registry := newWorkspaceInputRelayRegistry()
	done, ok := registry.start("exec", "exec-1")
	if !ok {
		t.Fatal("first relay start was rejected")
	}
	if _, ok := registry.start("exec", "exec-1"); ok {
		t.Fatal("duplicate live relay start was accepted")
	}
	if _, ok := registry.start("pty", "exec-1"); !ok {
		t.Fatal("different resource kind was rejected")
	}
	done()
	if _, ok := registry.start("exec", "exec-1"); !ok {
		t.Fatal("relay restart after completion was rejected")
	}
}

func TestReportWorkspaceInputRelayFailureIgnoresContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failures := make(chan error, 1)
	WorkspaceMaterializer{}.reportWorkspaceInputRelayFailure(ctx, failures, "exec", "exec-1", func() error {
		return context.Canceled
	})
	select {
	case err := <-failures:
		t.Fatalf("reported canceled relay failure: %v", err)
	default:
	}
}

type discardReadWriteCloser struct{}

func (discardReadWriteCloser) Read([]byte) (int, error)    { return 0, io.EOF }
func (discardReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardReadWriteCloser) Close() error                { return nil }

type blockingReadWriteCloser struct {
	once sync.Once
	done chan struct{}
}

func newBlockingReadWriteCloser() *blockingReadWriteCloser {
	return &blockingReadWriteCloser{done: make(chan struct{})}
}

func (c *blockingReadWriteCloser) Read([]byte) (int, error) {
	<-c.done
	return 0, io.EOF
}

func (c *blockingReadWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *blockingReadWriteCloser) Close() error {
	c.once.Do(func() {
		close(c.done)
	})
	return nil
}
