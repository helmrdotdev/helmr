package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/helmrdotdev/helmr/internal/workspaceop"
)

func testMaterializationArtifacts(t *testing.T) (*fakeCAS, api.WorkerWorkspaceMaterialization) {
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
	return store, api.WorkerWorkspaceMaterialization{
		BaseVersionID:              "version-1",
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

func TestWorkspaceMaterializerDispatchesNoopOperationToGuest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialClient, initialServer := net.Pipe()
	eventClient, eventServer := net.Pipe()
	operationClient, operationServer := net.Pipe()
	defer initialServer.Close()
	defer eventServer.Close()
	defer operationServer.Close()
	store, materialization := testMaterializationArtifacts(t)
	materialization.ID = "mat-1"
	materialization.OrgID = "org-1"
	materialization.WorkspaceID = "workspace-1"
	materialization.ReservationToken = "reservation-token"
	materialization.GuestdChannelToken = "channel-token"
	materialization.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		header, _, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read materialize header: %v", err)
			return
		}
		if header.Type != transport.StreamTypeWorkspaceMaterialize {
			t.Errorf("materialize stream type = %s", header.Type)
			return
		}
		var request workspacev0.MaterializeWorkspaceRequest
		if err := transport.ReadProtoFrame(initialServer, &request); err != nil {
			t.Errorf("read materialize request: %v", err)
			return
		}
		if got := request.GetEnvelope().GetChannelToken(); got != "channel-token" {
			t.Errorf("channel token = %q", got)
			return
		}
		if request.BaseVersionId != materialization.BaseVersionID || request.MountPath != materialization.WorkspaceMountPath || request.GetBaseArtifact().GetDigest() != materialization.WorkspaceArtifact.Digest || request.GetSandboxArtifact().GetDigest() != materialization.SandboxImageArtifact.Digest {
			t.Errorf("materialize request base_version_id=%q mount_path=%q base_digest=%q sandbox_digest=%q", request.BaseVersionId, request.MountPath, request.GetBaseArtifact().GetDigest(), request.GetSandboxArtifact().GetDigest())
			return
		}
		imageHeader, imageSize, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read sandbox image header: %v", err)
			return
		}
		if imageHeader.Type != transport.StreamTypeRunImage || imageHeader.WorkspaceID != materialization.WorkspaceID || int64(imageSize) != materialization.SandboxImageArtifact.SizeBytes {
			t.Errorf("sandbox image header = %+v size=%d", imageHeader, imageSize)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(imageSize)}); err != nil {
			t.Errorf("drain sandbox image: %v", err)
			return
		}
		artifactHeader, artifactSize, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read workspace artifact header: %v", err)
			return
		}
		if artifactHeader.Type != transport.StreamTypeWorkspaceArtifact || artifactHeader.WorkspaceID != materialization.WorkspaceID || int64(artifactSize) != materialization.WorkspaceArtifact.SizeBytes {
			t.Errorf("workspace artifact header = %+v size=%d", artifactHeader, artifactSize)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(artifactSize)}); err != nil {
			t.Errorf("drain workspace artifact: %v", err)
			return
		}
		_ = transport.WriteProtoFrame(initialServer, &workspacev0.MaterializeWorkspaceResponse{
			State:                  "running",
			GuestdChannelTokenHash: sha256sum.HexBytes([]byte("channel-token")),
		})
	}()
	go func() {
		header, _, err := transport.ReadStreamFrameHeader(eventServer)
		if err != nil {
			t.Errorf("read event header: %v", err)
			return
		}
		if header.Type != transport.StreamTypeWorkspaceEvents {
			t.Errorf("event header = %+v", header)
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := transport.ReadProtoFrame(eventServer, &envelope); err != nil {
			t.Errorf("read event envelope: %v", err)
			return
		}
		if envelope.GetChannelToken() != "channel-token" || envelope.GetMaterializationId() != "mat-1" {
			t.Errorf("event envelope = %+v", &envelope)
			return
		}
		<-ctx.Done()
	}()
	go func() {
		header, _, err := transport.ReadStreamFrameHeader(operationServer)
		if err != nil {
			t.Errorf("read operation header: %v", err)
			return
		}
		if header.Type != transport.StreamTypeWorkspaceOperation || header.OperationID != "operation-1" {
			t.Errorf("operation header = %+v", header)
			return
		}
		var request workspacev0.WorkspaceOperationRequest
		if err := transport.ReadProtoFrame(operationServer, &request); err != nil {
			t.Errorf("read operation request: %v", err)
			return
		}
		if request.OperationKind != "noop" || request.GetEnvelope().GetChannelToken() != "channel-token" || request.GetEnvelope().GetFencingToken() != "fence-1" || request.GetEnvelope().GetFencingGeneration() != 2 {
			t.Errorf("operation request kind=%q channel_token=%q fencing_token=%q fencing_generation=%d", request.OperationKind, request.GetEnvelope().GetChannelToken(), request.GetEnvelope().GetFencingToken(), request.GetEnvelope().GetFencingGeneration())
			return
		}
		_ = transport.WriteProtoFrame(operationServer, &workspacev0.WorkspaceOperationResult{ResultJson: `{"ok":true}`})
	}()
	client := &workspaceMaterializerTestClient{
		cancel:      cancel,
		startErrors: []error{errors.New("temporary start error")},
		operation: &api.WorkerWorkspaceOperation{
			WorkspaceOperationResponse: api.WorkspaceOperationResponse{
				ID:                 "operation-1",
				OrgID:              "org-1",
				WorkspaceID:        "workspace-1",
				MaterializationID:  "mat-1",
				OperationKind:      "noop",
				FencingToken:       "fence-1",
				FencingGeneration:  2,
				RequestFingerprint: testWorkspaceOperationFingerprint("noop", `{}`),
				OperationExpiresAt: time.Now().Add(time.Hour),
				Request:            []byte(`{}`),
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
	err := materializer.RunWorkspaceMaterialization(ctx, materialization, client)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("materializer err = %v, want context canceled", err)
	}
	if string(client.completed.Result) != `{"ok":true}` || client.completed.OperationID != "operation-1" || client.completed.ClaimToken != "claim-token" {
		t.Fatalf("completed operation = %+v", client.completed)
	}
	if len(client.claims) == 0 || client.claims[0].OrgID != "org-1" || client.claims[0].MaterializationID != "mat-1" || client.claims[0].ReservationToken != "reservation-token" {
		t.Fatalf("claim request = %+v", client.claims)
	}
	if len(client.starts) != 2 || client.starts[0].OperationID != "operation-1" || client.starts[1].OperationID != "operation-1" {
		t.Fatalf("start retries = %+v", client.starts)
	}
	if len(client.running) != 1 || client.running[0].OrgID != "org-1" || client.running[0].MaterializationID != "mat-1" || client.running[0].ReservationToken != "reservation-token" {
		t.Fatalf("running request = %+v", client.running)
	}
	if client.stops != 1 {
		t.Fatalf("stops = %d", client.stops)
	}
}

func TestWorkspaceMaterializerRejectsMismatchedClaimedOperation(t *testing.T) {
	materializer := WorkspaceMaterializer{}
	_, err := materializer.dispatchOperation(context.Background(), nil, api.WorkerWorkspaceMaterialization{
		ID:          "mat-1",
		WorkspaceID: "workspace-1",
	}, api.WorkerWorkspaceOperation{
		WorkspaceOperationResponse: api.WorkspaceOperationResponse{
			ID:                 "operation-1",
			OrgID:              "org-1",
			WorkspaceID:        "workspace-2",
			MaterializationID:  "mat-2",
			OperationKind:      "noop",
			RequestFingerprint: testWorkspaceOperationFingerprint("noop", `{}`),
			OperationExpiresAt: time.Now().Add(time.Hour),
		},
		ClaimToken: "claim-token",
	})
	if err == nil {
		t.Fatal("expected mismatched claimed operation to fail before guest dispatch")
	}
}

func TestWorkspaceMaterializerCleansPartialArtifactsOnMaterializeFailure(t *testing.T) {
	ctx := context.Background()
	store, materialization := testMaterializationArtifacts(t)
	materialization.ID = "mat-1"
	materialization.OrgID = "org-1"
	materialization.WorkspaceID = "workspace-1"
	materialization.ReservationToken = "reservation-token"
	materialization.WorkspaceArtifact.SizeBytes++
	tempDir := t.TempDir()
	client := &workspaceMaterializerTestClient{}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{},
		CAS:       store,
		TempDir:   tempDir,
	}
	err := materializer.RunWorkspaceMaterialization(ctx, materialization, client)
	if err == nil {
		t.Fatal("expected materialization failure")
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial materialization temp files were not cleaned up: %+v", entries)
	}
}

func TestWorkspaceMaterializerFailsStartupWhenGuestDoesNotRegister(t *testing.T) {
	ctx := context.Background()
	initialClient, initialServer := net.Pipe()
	defer initialServer.Close()
	store, materialization := testMaterializationArtifacts(t)
	materialization.ID = "mat-1"
	materialization.OrgID = "org-1"
	materialization.WorkspaceID = "workspace-1"
	materialization.ReservationToken = "reservation-token"
	materialization.GuestdChannelToken = "channel-token"
	materialization.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		_, _, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil {
			return
		}
		var request workspacev0.MaterializeWorkspaceRequest
		if err := transport.ReadProtoFrame(initialServer, &request); err != nil {
			return
		}
		imageHeader, imageSize, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil || imageHeader.Type != transport.StreamTypeRunImage {
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(imageSize)}); err != nil {
			return
		}
		artifactHeader, artifactSize, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil || artifactHeader.Type != transport.StreamTypeWorkspaceArtifact {
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
			operation: noopReadWriteCloser{},
		}},
		CAS:            store,
		TempDir:        t.TempDir(),
		Heartbeat:      time.Hour,
		StartupTimeout: time.Millisecond,
	}
	err := materializer.RunWorkspaceMaterialization(ctx, materialization, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("materializer err = %v, want deadline exceeded", err)
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_materialization_startup_timeout") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestWorkspaceMaterializerFailsMaterializationOnFatalHeartbeatError(t *testing.T) {
	ctx := context.Background()
	initialClient, initialServer := net.Pipe()
	defer initialServer.Close()
	store, materialization := testMaterializationArtifacts(t)
	materialization.ID = "mat-1"
	materialization.OrgID = "org-1"
	materialization.WorkspaceID = "workspace-1"
	materialization.ReservationToken = "reservation-token"
	materialization.GuestdChannelToken = "channel-token"
	materialization.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgeMaterialization(t, initialServer, materialization)
	client := &workspaceMaterializerTestClient{
		renewErrors: []error{errors.New("renew failed")},
	}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{
			initial:   initialClient,
			streams:   []io.ReadWriteCloser{newBlockingReadWriteCloser()},
			operation: noopReadWriteCloser{},
		}},
		CAS:       store,
		TempDir:   t.TempDir(),
		Heartbeat: 10 * time.Millisecond,
		PollEvery: time.Hour,
	}
	err := materializer.RunWorkspaceMaterialization(ctx, materialization, client)
	if err == nil || !strings.Contains(err.Error(), "renew workspace materialization") {
		t.Fatalf("materializer err = %v, want renew error", err)
	}
	if len(client.renews) == 0 || client.renews[0].OrgID != "org-1" || client.renews[0].MaterializationID != "mat-1" || client.renews[0].ReservationToken != "reservation-token" {
		t.Fatalf("renew requests = %+v", client.renews)
	}
	if len(client.failures) != 1 || client.failures[0].MaterializationID != "mat-1" || client.failures[0].ReservationToken != "reservation-token" {
		t.Fatalf("failures = %+v", client.failures)
	}
}

func TestWorkspaceMaterializerFailsMaterializationWhenSessionExits(t *testing.T) {
	ctx := context.Background()
	initialClient, initialServer := net.Pipe()
	defer initialServer.Close()
	exit := make(chan error, 1)
	store, materialization := testMaterializationArtifacts(t)
	materialization.ID = "mat-1"
	materialization.OrgID = "org-1"
	materialization.WorkspaceID = "workspace-1"
	materialization.ReservationToken = "reservation-token"
	materialization.GuestdChannelToken = "channel-token"
	materialization.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		acknowledgeMaterialization(t, initialServer, materialization)
		exit <- errors.New("firecracker exited")
	}()
	client := &workspaceMaterializerTestClient{}
	materializer := WorkspaceMaterializer{
		Connector: workspaceMaterializerTestConnector{session: &workspaceMaterializerTestSession{
			initial:   initialClient,
			streams:   []io.ReadWriteCloser{newBlockingReadWriteCloser()},
			operation: noopReadWriteCloser{},
			exit:      exit,
		}},
		CAS:       store,
		TempDir:   t.TempDir(),
		Heartbeat: time.Hour,
		PollEvery: time.Hour,
	}
	err := materializer.RunWorkspaceMaterialization(ctx, materialization, client)
	if err == nil || !strings.Contains(err.Error(), "workspace materialization VM exited") {
		t.Fatalf("materializer err = %v, want VM exit", err)
	}
	if len(client.failures) != 1 || client.failures[0].MaterializationID != "mat-1" || client.failures[0].ReservationToken != "reservation-token" {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_materialization_vm_exited") {
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
	store, materialization := testMaterializationArtifacts(t)
	materialization.ID = "mat-1"
	materialization.OrgID = "org-1"
	materialization.WorkspaceID = "workspace-1"
	materialization.ReservationToken = "reservation-token"
	materialization.GuestdChannelToken = "channel-token"
	materialization.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		_, _, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read materialize header: %v", err)
			return
		}
		var request workspacev0.MaterializeWorkspaceRequest
		if err := transport.ReadProtoFrame(initialServer, &request); err != nil {
			t.Errorf("read materialize request: %v", err)
			return
		}
		imageHeader, imageSize, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read sandbox image header: %v", err)
			return
		}
		if imageHeader.Type != transport.StreamTypeRunImage {
			t.Errorf("sandbox image header = %+v", imageHeader)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(imageSize)}); err != nil {
			t.Errorf("drain sandbox image: %v", err)
			return
		}
		artifactHeader, artifactSize, err := transport.ReadStreamFrameHeader(initialServer)
		if err != nil {
			t.Errorf("read workspace artifact header: %v", err)
			return
		}
		if artifactHeader.Type != transport.StreamTypeWorkspaceArtifact {
			t.Errorf("workspace artifact header = %+v", artifactHeader)
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: initialServer, N: int64(artifactSize)}); err != nil {
			t.Errorf("drain workspace artifact: %v", err)
			return
		}
		_ = transport.WriteProtoFrame(initialServer, &workspacev0.MaterializeWorkspaceResponse{
			State:                  "running",
			GuestdChannelTokenHash: sha256sum.HexBytes([]byte("channel-token")),
		})
	}()
	go func() {
		_, _, err := transport.ReadStreamFrameHeader(eventServer)
		if err != nil {
			t.Errorf("read event header: %v", err)
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := transport.ReadProtoFrame(eventServer, &envelope); err != nil {
			t.Errorf("read event envelope: %v", err)
			return
		}
		<-ctx.Done()
	}()
	go func() {
		_, _, err := transport.ReadStreamFrameHeader(operationServer)
		if err != nil {
			t.Errorf("read operation header: %v", err)
			return
		}
		var request workspacev0.WorkspaceOperationRequest
		if err := transport.ReadProtoFrame(operationServer, &request); err != nil {
			t.Errorf("read operation request: %v", err)
			return
		}
		_ = transport.WriteProtoFrame(operationServer, &workspacev0.WorkspaceOperationResult{ResultJson: `{"ok":true}`})
	}()
	client := &workspaceMaterializerTestClient{
		cancel:           cancel,
		completionErrors: []error{errors.New("transient completion failure")},
		operation: &api.WorkerWorkspaceOperation{
			WorkspaceOperationResponse: api.WorkspaceOperationResponse{
				ID:                 "operation-1",
				OrgID:              "org-1",
				WorkspaceID:        "workspace-1",
				MaterializationID:  "mat-1",
				OperationKind:      "noop",
				FencingGeneration:  1,
				RequestFingerprint: testWorkspaceOperationFingerprint("noop", `{}`),
				OperationExpiresAt: time.Now().Add(time.Hour),
				Request:            []byte(`{}`),
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
	err := materializer.RunWorkspaceMaterialization(ctx, materialization, client)
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
	materialization := api.WorkerWorkspaceMaterialization{
		ID:               "mat-1",
		OrgID:            "org-1",
		ProjectID:        "project-1",
		EnvironmentID:    "env-1",
		WorkspaceID:      "workspace-1",
		ReservationToken: "reservation-token",
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
		if err := materializer.persistWorkspaceOperationEvent(context.Background(), client, materialization, persistState, event); err != nil {
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
	materialization := api.WorkerWorkspaceMaterialization{
		ID:               "mat-1",
		OrgID:            "org-1",
		ProjectID:        "project-1",
		EnvironmentID:    "env-1",
		WorkspaceID:      "workspace-1",
		ReservationToken: "reservation-token",
	}
	event := &workspacev0.WorkspaceOperationEvent{
		Event: &workspacev0.WorkspaceOperationEvent_ExecStdoutChunk{
			ExecStdoutChunk: &workspacev0.WorkspaceExecOutputChunk{ExecId: "exec-1", Data: []byte("hello")},
		},
	}
	state := newWorkspaceOperationEventPersistState()
	err := retryWorkspaceOperation(context.Background(), materializer.CompleteErrorBackoff, func() error {
		return materializer.persistWorkspaceOperationEvent(context.Background(), client, materialization, state, event)
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

func acknowledgeMaterialization(t *testing.T, stream io.ReadWriteCloser, materialization api.WorkerWorkspaceMaterialization) {
	t.Helper()
	_, _, err := transport.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read materialize header: %v", err)
		return
	}
	var request workspacev0.MaterializeWorkspaceRequest
	if err := transport.ReadProtoFrame(stream, &request); err != nil {
		t.Errorf("read materialize request: %v", err)
		return
	}
	imageHeader, imageSize, err := transport.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read sandbox image header: %v", err)
		return
	}
	if imageHeader.Type != transport.StreamTypeRunImage {
		t.Errorf("sandbox image header = %+v", imageHeader)
		return
	}
	if _, err := io.Copy(io.Discard, &io.LimitedReader{R: stream, N: int64(imageSize)}); err != nil {
		t.Errorf("drain sandbox image: %v", err)
		return
	}
	artifactHeader, artifactSize, err := transport.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read workspace artifact header: %v", err)
		return
	}
	if artifactHeader.Type != transport.StreamTypeWorkspaceArtifact {
		t.Errorf("workspace artifact header = %+v", artifactHeader)
		return
	}
	if _, err := io.Copy(io.Discard, &io.LimitedReader{R: stream, N: int64(artifactSize)}); err != nil {
		t.Errorf("drain workspace artifact: %v", err)
		return
	}
	_ = transport.WriteProtoFrame(stream, &workspacev0.MaterializeWorkspaceResponse{
		State:                  "running",
		GuestdChannelTokenHash: materialization.GuestdChannelTokenHash,
	})
}

type workspaceMaterializerTestConnector struct {
	session vm.Session
}

func (c workspaceMaterializerTestConnector) Connect(context.Context, compute.NetworkPolicy) (vm.Session, error) {
	return c.session, nil
}

func (c workspaceMaterializerTestConnector) Materialize(_ context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	if request.RootfsDigest == "" || request.ImageDigest == "" || request.ImageFormat != "oci-tar" || request.WorkspaceArtifactPath == "" || request.BaseVersionID == "" {
		return nil, errors.New("materialize request missing artifact authority")
	}
	return c.session, nil
}

type workspaceMaterializerTestSession struct {
	initial   io.ReadWriteCloser
	operation io.ReadWriteCloser
	streams   []io.ReadWriteCloser
	opened    []io.ReadWriteCloser
	exit      <-chan error
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
	return nil
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
	renews           []api.WorkerWorkspaceMaterializationRenewRequest
	running          []api.WorkerWorkspaceMaterializationRunningRequest
	claims           []api.WorkerWorkspaceOperationClaimRequest
	starts           []api.WorkerWorkspaceOperationStartRequest
	startErrors      []error
	stops            int
	failures         []api.WorkerWorkspaceMaterializationFailRequest
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

func (c *workspaceMaterializerTestClient) RenewWorkspaceMaterialization(_ context.Context, request api.WorkerWorkspaceMaterializationRenewRequest) (api.WorkspaceMaterializationResponse, error) {
	c.renews = append(c.renews, request)
	if len(c.renewErrors) > 0 {
		err := c.renewErrors[0]
		c.renewErrors = c.renewErrors[1:]
		return api.WorkspaceMaterializationResponse{}, err
	}
	return api.WorkspaceMaterializationResponse{State: "materializing"}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspaceMaterializationRunning(_ context.Context, request api.WorkerWorkspaceMaterializationRunningRequest) (api.WorkspaceMaterializationResponse, error) {
	c.running = append(c.running, request)
	return api.WorkspaceMaterializationResponse{State: "running"}, nil
}

func (c *workspaceMaterializerTestClient) StopWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationStopRequest) (api.WorkspaceMaterializationResponse, error) {
	c.stops++
	return api.WorkspaceMaterializationResponse{State: "stopped"}, nil
}

func (c *workspaceMaterializerTestClient) FailWorkspaceMaterialization(_ context.Context, request api.WorkerWorkspaceMaterializationFailRequest) (api.WorkspaceMaterializationResponse, error) {
	c.failures = append(c.failures, request)
	return api.WorkspaceMaterializationResponse{State: "failed"}, nil
}

func (c *workspaceMaterializerTestClient) ClaimWorkspaceMaterializationOperation(_ context.Context, request api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error) {
	c.claims = append(c.claims, request)
	if c.operation == nil {
		return api.WorkerWorkspaceOperationClaimResponse{}, nil
	}
	operation := c.operation
	c.operation = nil
	return api.WorkerWorkspaceOperationClaimResponse{Operation: operation}, nil
}

func (c *workspaceMaterializerTestClient) StartWorkspaceMaterializationOperation(_ context.Context, request api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error) {
	c.starts = append(c.starts, request)
	if len(c.startErrors) > 0 {
		err := c.startErrors[0]
		c.startErrors = c.startErrors[1:]
		return api.WorkspaceOperationResponse{}, err
	}
	return api.WorkspaceOperationResponse{ID: request.OperationID, State: "running"}, nil
}

func (c *workspaceMaterializerTestClient) CompleteWorkspaceMaterializationOperation(_ context.Context, request api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error) {
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
	fingerprint, err := workspaceop.CanonicalRequestFingerprint(operationKind, []byte(requestJSON))
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
		_, bodyLen, err := transport.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := transport.ReadProtoFrame(guestStream, &envelope); err != nil {
			guestErr <- err
			return
		}
		if envelope.GetFencingGeneration() != 2 || envelope.GetWriteLeaseId() != "write-lease-1" || envelope.GetFencingToken() != "write-token-1" {
			guestErr <- fmt.Errorf("input envelope generation=%d write_lease=%q fencing_token=%q", envelope.GetFencingGeneration(), envelope.GetWriteLeaseId(), envelope.GetFencingToken())
			return
		}
		for expected := uint64(0); expected < 101; expected++ {
			var frame workspacev0.WorkspaceInputFrame
			if err := transport.ReadProtoFrame(guestStream, &frame); err != nil {
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
			if err := transport.WriteProtoFrame(guestStream, &workspacev0.WorkspaceStreamAck{
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
		if err := transport.ReadProtoFrame(guestStream, &frame); err != nil {
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
		if err := transport.WriteProtoFrame(guestStream, &workspacev0.WorkspaceStreamAck{
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
	materialization := api.WorkerWorkspaceMaterialization{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		ReservationToken:   "reservation-token",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		MaterializationID:  "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "exec-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspaceExecInputRelay(context.Background(), session, materialization, operation, "exec-1", control); err != nil {
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
		_, bodyLen, err := transport.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := transport.ReadProtoFrame(guestStream, &envelope); err != nil {
			guestErr <- err
			return
		}
		var frame workspacev0.WorkspaceInputFrame
		if err := transport.ReadProtoFrame(guestStream, &frame); err != nil {
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
		guestErr <- transport.WriteProtoFrame(guestStream, &workspacev0.WorkspaceStreamAck{
			ResourceKind:  closeFrame.GetResourceKind(),
			ResourceId:    closeFrame.GetResourceId(),
			Stream:        closeFrame.GetStream(),
			DurableOffset: closeFrame.GetOffset(),
		})
	}()
	session := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{clientStream}}
	materialization := api.WorkerWorkspaceMaterialization{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		ReservationToken:   "reservation-token",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		MaterializationID:  "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "exec-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspaceExecInputRelay(context.Background(), session, materialization, operation, "exec-1", control); err != nil {
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
		_, bodyLen, err := transport.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := transport.ReadProtoFrame(guestStream, &envelope); err != nil {
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
	materialization := api.WorkerWorkspaceMaterialization{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		ReservationToken:   "reservation-token",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		MaterializationID:  "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "exec-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspaceExecInputRelay(context.Background(), session, materialization, operation, "exec-1", control); err != nil {
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
		_, bodyLen, err := transport.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestErr <- err
			return
		}
		if bodyLen != 0 {
			guestErr <- errors.New("workspace input stream header had a body")
			return
		}
		var envelope workspacev0.WorkspaceOperationEnvelope
		if err := transport.ReadProtoFrame(guestStream, &envelope); err != nil {
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
	materialization := api.WorkerWorkspaceMaterialization{
		ID:                 "mat-1",
		OrgID:              "org-1",
		ProjectID:          "project-1",
		EnvironmentID:      "env-1",
		WorkspaceID:        "workspace-1",
		ReservationToken:   "reservation-token",
		GuestdChannelToken: "guest-token",
		FencingGeneration:  1,
	}
	operation := api.WorkerWorkspaceOperation{WorkspaceOperationResponse: api.WorkspaceOperationResponse{
		ID:                 "op-1",
		MaterializationID:  "mat-1",
		WorkspaceID:        "workspace-1",
		ResourceID:         "pty-1",
		FencingGeneration:  2,
		WriteLeaseID:       "write-lease-1",
		FencingToken:       "write-token-1",
		OperationExpiresAt: time.Now().Add(time.Minute),
		RequestFingerprint: "fingerprint-1",
	}}
	if err := (WorkspaceMaterializer{}).runWorkspacePtyInputRelay(context.Background(), session, materialization, operation, "pty-1", control); err != nil {
		t.Fatalf("run pty input relay: %v", err)
	}
	if err := <-guestErr; err != nil {
		t.Fatalf("guest stream: %v", err)
	}
	if len(control.ptyDelivered) != 0 {
		t.Fatalf("pty delivered calls = %d, want 0", len(control.ptyDelivered))
	}
}

func TestReportWorkspaceInputRelayFailureEmitsMaterializationFailure(t *testing.T) {
	failures := make(chan error, 1)
	WorkspaceMaterializer{}.reportWorkspaceInputRelayFailure(context.Background(), failures, "exec", "exec-1", func() error {
		return errors.New("ack failed")
	})
	select {
	case err := <-failures:
		var failure materializationFailure
		if !errors.As(err, &failure) {
			t.Fatalf("failure type = %T, want materializationFailure", err)
		}
		if failure.code != "workspace_materialization_input_stream_lost" {
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
	failures <- materializationFailure{code: "pending", err: errors.New("pending")}
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

type noopReadWriteCloser struct{}

func (noopReadWriteCloser) Read([]byte) (int, error)    { return 0, io.EOF }
func (noopReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (noopReadWriteCloser) Close() error                { return nil }

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
