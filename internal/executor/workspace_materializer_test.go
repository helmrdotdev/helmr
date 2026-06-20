package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workspace"
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
	operationClient, operationServer := net.Pipe()
	defer initialServer.Close()
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
			initial:   initialClient,
			operation: operationClient,
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
	operationClient, operationServer := net.Pipe()
	defer initialServer.Close()
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
			initial:   initialClient,
			operation: operationClient,
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
	exit      <-chan error
}

func (s *workspaceMaterializerTestSession) Stream() io.ReadWriteCloser {
	return s.initial
}

func (s *workspaceMaterializerTestSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return s.operation, nil
}

func (s *workspaceMaterializerTestSession) Close(context.Context) error {
	_ = s.initial.Close()
	_ = s.operation.Close()
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

func testWorkspaceOperationFingerprint(operationKind string, requestJSON string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(operationKind)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strings.TrimSpace(requestJSON)))
	return hex.EncodeToString(hash.Sum(nil))
}

type noopReadWriteCloser struct{}

func (noopReadWriteCloser) Read([]byte) (int, error)    { return 0, io.EOF }
func (noopReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (noopReadWriteCloser) Close() error                { return nil }
