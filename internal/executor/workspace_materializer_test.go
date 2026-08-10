package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/localcache"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestWorkspaceMaterializerUsesStartupTimeout(t *testing.T) {
	if got := (WorkspaceMaterializer{}).startupTimeout(); got != workspaceStartupTimeout {
		t.Fatalf("startup timeout = %s, want %s", got, workspaceStartupTimeout)
	}
	custom := time.Second
	if got := (WorkspaceMaterializer{StartupTimeout: custom}).startupTimeout(); got != custom {
		t.Fatalf("custom startup timeout = %s, want %s", got, custom)
	}
}

func testWorkspaceMountArtifacts(t *testing.T) (*fakeCAS, workerapi.WorkspaceMount) {
	t.Helper()
	store := &fakeCAS{objects: map[string][]byte{}}
	imageObject, err := store.Put(context.Background(), deployment.WorkspaceImageArtifactMediaType, strings.NewReader("oci image"))
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
	return store, workerapi.WorkspaceMount{
		BaseVersionID:     "version-1",
		RuntimeInstanceID: "runtime-instance-1",
		RuntimeEpoch:      1,
		RuntimeIdentityID: "runtime-1",
		WorkspaceImage: workerapi.CASObject{
			Digest: imageObject.Digest, SizeBytes: imageObject.SizeBytes, MediaType: imageObject.MediaType,
		},
		RootfsDigest: "sha256:runtime-rootfs",
		WorkspaceArtifact: workerapi.WorkspaceArtifact{
			Digest:     workspaceObject.Digest,
			MediaType:  workspaceObject.MediaType,
			Encoding:   workspace.ArtifactEncoding,
			SizeBytes:  workspaceObject.SizeBytes,
			EntryCount: int32(workspaceArtifact.EntryCount),
		},
		WorkspaceMountPath: "/workspace",
	}
}

func workspacePreparedRuntimePool(t *testing.T, mount workerapi.WorkspaceMount, session vm.Session) *PreparedRuntimePool {
	t.Helper()
	target := runtimeCapacityTarget(mount.RuntimeInstanceID, mount.RuntimeEpoch)
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	key := runtimeInstanceIDFromWorkspaceMount(mount)
	ready := newPreparedRuntimeSignal()
	ready.finish(nil)
	pool.entries[key] = []preparedRuntimeEntry{{
		session: session, poolKey: key, runtimeInstanceID: target.ID,
		runtimeEpoch: target.WorkerEpoch, target: target,
		exit: newPreparedRuntimeSignal(), ready: ready,
	}}
	return pool
}

func TestWorkspaceMaterializerRestoreCASObjectUsesLocalCache(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	cacheDir := t.TempDir()
	tempDir := t.TempDir()
	materializer := WorkspaceMaterializer{
		CAS:              store,
		ArtifactCacheDir: cacheDir,
	}

	_, firstCleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "workspace-image", workspaceMount.WorkspaceImage)
	if err != nil {
		t.Fatal(err)
	}
	firstCleanup()
	secondPath, secondCleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "workspace-image", workspaceMount.WorkspaceImage)
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
	if got := store.getCalls[workspaceMount.WorkspaceImage.Digest]; got != 1 {
		t.Fatalf("CAS Get calls = %d, want 1", got)
	}
}

func TestWorkspaceMaterializerRestoreCASObjectRefreshesInvalidLocalCache(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	cacheDir := t.TempDir()
	tempDir := t.TempDir()
	cachePath, err := artifactCachePath(cacheDir, workspaceMount.WorkspaceImage.Digest)
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

	path, cleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "workspace-image", workspaceMount.WorkspaceImage)
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
	if got := store.getCalls[workspaceMount.WorkspaceImage.Digest]; got != 1 {
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

func TestWorkspaceMaterializerChecksOutPreparedRuntime(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	wantSession := &workspaceMaterializerTestSession{}
	pool := workspacePreparedRuntimePool(t, workspaceMount, wantSession)
	materializer := WorkspaceMaterializer{
		CAS:         store,
		TempDir:     t.TempDir(),
		RuntimePool: pool,
	}

	session, workspacePath, cleanup, runtimeInstanceID, err := materializer.materializeSession(context.Background(), &workspaceMount)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if session != wantSession {
		t.Fatalf("session = %T %p, want %T %p", session, session, wantSession, wantSession)
	}
	if strings.TrimSpace(workspacePath) == "" {
		t.Fatal("workspace artifact path is empty")
	}
	if runtimeInstanceID != workspaceMount.RuntimeInstanceID {
		t.Fatalf("runtime instance id = %q, want %q", runtimeInstanceID, workspaceMount.RuntimeInstanceID)
	}
	if !pool.runtimeCheckedOut(workspaceMount.RuntimeInstanceID, workspaceMount.RuntimeEpoch) {
		t.Fatal("prepared runtime was not checked out")
	}
	if got := store.getCalls[workspaceMount.WorkspaceImage.Digest]; got != 0 {
		t.Fatalf("workspace image CAS gets = %d, want 0", got)
	}
	if got := store.getCalls[workspaceMount.WorkspaceArtifact.Digest]; got != 1 {
		t.Fatalf("workspace artifact CAS gets = %d, want 1", got)
	}
	if err := pool.ReleaseCheckout(workspaceMount.RuntimeInstanceID, workspaceMount.RuntimeEpoch); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceMountPhaseErrorUsesLatestGuestError(t *testing.T) {
	got := workspaceMountPhaseError([]*workspacev0.WorkspaceMountPhase{
		{Name: "guest_workspace_image_restore"},
		{Name: "guest_workspace_artifact_restore", Error: "extract workspace artifact: permission denied"},
	})
	if got != "guest_workspace_artifact_restore: extract workspace artifact: permission denied" {
		t.Fatalf("phase error = %q", got)
	}
}

func TestWorkspaceMaterializerFailsWhenPreparedRuntimeIsMissing(t *testing.T) {
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	materializer := WorkspaceMaterializer{
		CAS:         store,
		RuntimePool: NewPreparedRuntimePool(nil, nil, 1, nil),
	}
	client := &workspaceMaterializerTestClient{}

	err := materializer.RunWorkspaceMount(context.Background(), workspaceMount, client)
	if err == nil {
		t.Fatal("missing prepared runtime was accepted")
	}
	var failure workspaceMountFailure
	if !errors.As(err, &failure) || failure.code != "workspace_runtime_not_prepared" {
		t.Fatalf("error = %v, want workspace_runtime_not_prepared", err)
	}
	if len(client.failures) != 1 {
		t.Fatalf("workspace mount failures = %d, want 1", len(client.failures))
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(client.failures[0].Error, &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "workspace_runtime_not_prepared" {
		t.Fatalf("workspace mount failure code = %q, want workspace_runtime_not_prepared", body.Code)
	}
	if got := store.getCalls[workspaceMount.WorkspaceImage.Digest]; got != 0 {
		t.Fatalf("workspace image CAS gets = %d, want 0", got)
	}
	if got := store.getCalls[workspaceMount.WorkspaceArtifact.Digest]; got != 0 {
		t.Fatalf("workspace artifact CAS gets = %d, want 0", got)
	}
}

func TestWorkspaceMaterializerCanonicalEmptyRootSkipsWorkspaceCAS(t *testing.T) {
	store, mount := testWorkspaceMountArtifacts(t)
	mount.WorkspaceArtifact = workerapi.WorkspaceArtifact{
		MediaType: workspace.ArtifactMediaType,
		Encoding:  workspace.ArtifactEncoding,
	}
	session := &workspaceMaterializerTestSession{}
	pool := workspacePreparedRuntimePool(t, mount, session)
	materializer := WorkspaceMaterializer{
		CAS:         store,
		RuntimePool: pool,
	}

	gotSession, workspacePath, cleanup, _, err := materializer.materializeSession(context.Background(), &mount)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if gotSession != session {
		t.Fatalf("session = %v, want prepared session", gotSession)
	}
	if workspacePath != "" {
		t.Fatalf("empty root workspace path = %q, want empty", workspacePath)
	}
	if got := store.getCalls[mount.WorkspaceImage.Digest]; got != 0 {
		t.Fatalf("workspace image CAS gets = %d, want 0", got)
	}
	if err := pool.ReleaseCheckout(mount.RuntimeInstanceID, mount.RuntimeEpoch); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceMaterializerDispatchesBasicExec(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	session := &workspaceMaterializerTestSession{operation: clientStream}
	secretValue := []byte("secret-value")
	exec := workerapi.WorkspaceExec{
		ProcessID:           "process-1",
		WorkspaceID:         "workspace-1",
		WorkspaceMountID:    "mount-1",
		RequestFingerprint:  strings.Repeat("a", 64),
		Request:             json.RawMessage(`{"command":["sh","-c","printf ok"],"cwd":"/workspace","env":{},"timeout_ms":1000}`),
		Stdin:               []byte("input"),
		Secrets:             []workerapi.SecretDelivery{{Env: &workerapi.SecretEnv{Name: "TOKEN"}, Value: secretValue}},
		WorkspaceLeaseID:    "lease-1",
		WriteCapability:     "capability-1",
		FencingGeneration:   2,
		OwnershipGeneration: 3,
		WriterGeneration:    4,
		ExpiresAt:           time.Now().Add(time.Minute),
	}
	guestDone := make(chan error, 1)
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestDone <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceBasicExec ||
			header.OperationID != exec.ProcessID {
			guestDone <- fmt.Errorf("unexpected header: %+v", header)
			return
		}
		var request workspacev0.WorkspaceBasicExecRequest
		if err := frameio.ReadProtoFrame(guestStream, &request); err != nil {
			guestDone <- err
			return
		}
		if request.GetEnvelope().GetChannelToken() != "channel-token" ||
			request.GetEnvelope().GetFencingToken() != exec.WriteCapability ||
			string(request.GetStdin()) != "input" ||
			len(request.GetSecrets()) != 1 ||
			request.GetSecrets()[0].GetPlacementKind() != "env" ||
			request.GetSecrets()[0].GetPlacementTarget() != "TOKEN" ||
			string(request.GetSecrets()[0].GetValue()) != "secret-value" {
			guestDone <- fmt.Errorf("unexpected BasicExec request: %+v", &request)
			return
		}
		guestDone <- frameio.WriteProtoFrame(guestStream, &workspacev0.WorkspaceBasicExecResult{
			ExitCode:           7,
			Stdout:             []byte("stdout"),
			Stderr:             []byte("stderr"),
			Outcome:            "exited",
			RequestFingerprint: exec.RequestFingerprint,
		})
	}()
	completion, err := (WorkspaceMaterializer{}).dispatchWorkspaceBasicExec(
		context.Background(),
		session,
		workerapi.WorkspaceMount{
			ID: "mount-1", OrgID: "org-1", WorkspaceID: "workspace-1",
			GuestdChannelToken: "channel-token",
		},
		exec,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-guestDone; err != nil {
		t.Fatal(err)
	}
	if completion.ProcessID != exec.ProcessID ||
		completion.WorkspaceLeaseID != exec.WorkspaceLeaseID ||
		completion.ExitCode == nil ||
		*completion.ExitCode != 7 ||
		string(completion.Stdout) != "stdout" ||
		string(completion.Stderr) != "stderr" ||
		completion.Outcome != "exited" {
		t.Fatalf("completion = %+v", completion)
	}
	for _, value := range secretValue {
		if value != 0 {
			t.Fatal("Secret plaintext was not cleared after dispatch")
		}
	}
}

func TestWorkspaceMaterializerRejectsMismatchedBasicExecClaim(t *testing.T) {
	_, err := (WorkspaceMaterializer{}).dispatchWorkspaceBasicExec(
		context.Background(),
		&workspaceMaterializerTestSession{},
		workerapi.WorkspaceMount{
			ID: "mount-1", WorkspaceID: "workspace-1",
			GuestdChannelToken: "channel-token",
		},
		workerapi.WorkspaceExec{
			ProcessID: "process-1", WorkspaceMountID: "mount-2",
			WorkspaceID: "workspace-1", RequestFingerprint: strings.Repeat("a", 64),
			WorkspaceLeaseID: "lease-1", WriteCapability: "capability-1",
			FencingGeneration: 1, OwnershipGeneration: 1, WriterGeneration: 1,
			ExpiresAt: time.Now().Add(time.Minute),
		},
	)
	var protocolError *workspaceBasicExecProtocolError
	if !errors.As(err, &protocolError) {
		t.Fatalf("error = %v, want protocol error", err)
	}
}

func TestWorkspaceMaterializerRejectsGuestAuthorityOutcomes(t *testing.T) {
	for _, outcome := range []string{
		"workspace_exec_fenced",
		"workspace_exec_expired",
		"workspace_exec_invalid",
		"workspace_exec_fingerprint_conflict",
		"workspace_exec_unavailable",
		"future_outcome",
		"",
	} {
		t.Run(outcome, func(t *testing.T) {
			var protocolError *workspaceBasicExecProtocolError
			if err := validateWorkspaceBasicExecOutcome(outcome); !errors.As(err, &protocolError) {
				t.Fatalf("error = %v, want protocol error", err)
			}
		})
	}
}

func TestWorkspaceMaterializerCompletionStopsOnNonRetryableError(t *testing.T) {
	client := &workspaceMaterializerTestClient{
		completeErrors: []error{workspaceMaterializerHTTPError(http.StatusBadRequest)},
	}
	_, err := (WorkspaceMaterializer{
		CompleteErrorBackoff: time.Nanosecond,
	}).completeWorkspaceBasicExec(
		context.Background(),
		client,
		workerapi.WorkspaceExecCompleteRequest{},
	)
	if err == nil {
		t.Fatal("non-retryable completion error was ignored")
	}
	if len(client.execCompletions) != 1 {
		t.Fatalf("completion attempts = %d, want 1", len(client.execCompletions))
	}
}

func TestWorkspaceMaterializerCompletionRetriesServerError(t *testing.T) {
	client := &workspaceMaterializerTestClient{
		completeErrors: []error{
			workspaceMaterializerHTTPError(http.StatusServiceUnavailable),
		},
	}
	_, err := (WorkspaceMaterializer{
		CompleteErrorBackoff: time.Nanosecond,
	}).completeWorkspaceBasicExec(
		context.Background(),
		client,
		workerapi.WorkspaceExecCompleteRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.execCompletions) != 2 {
		t.Fatalf("completion attempts = %d, want 2", len(client.execCompletions))
	}
}

func TestWorkspaceMaterializerStopWorkspaceGuestStoresCapturedArtifact(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	store := &fakeCAS{objects: map[string][]byte{}}
	body := []byte("captured workspace")
	object := store.put(workspace.ArtifactMediaType, body)
	session := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{client}}
	workspaceMount := workerapi.WorkspaceMount{
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
	err := (WorkspaceMaterializer{CAS: store}).stopControlledWorkspaceMount(context.Background(), session, workspaceMount, workerapi.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9}, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.stops != 1 {
		t.Fatalf("stops = %d, want 1", client.stops)
	}
	if closed := session.closeCount(); closed != 1 {
		t.Fatalf("session closes = %d, want 1 before reporting the mount stopped", closed)
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
	err := (WorkspaceMaterializer{CAS: store}).stopControlledWorkspaceMount(context.Background(), session, workspaceMount, workerapi.WorkspaceMountResponse{State: "unmounting"}, client)
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
	}, workspaceMount, workerapi.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9}, client)
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
	workspaceMount := workerapi.WorkspaceMount{
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
	}, workspaceMount, workerapi.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9, DirtyGeneration: 3}, client)
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
	workspaceMount := workerapi.WorkspaceMount{
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
	}, workspaceMount, workerapi.WorkspaceMountResponse{State: "unmounting", FencingGeneration: 9, DirtyGeneration: 3}, client)
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
	pool := workspacePreparedRuntimePool(t, workspaceMount, &workspaceMaterializerTestSession{})
	materializer := WorkspaceMaterializer{
		CAS:         store,
		TempDir:     tempDir,
		RuntimePool: pool,
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
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		_, _, err := wire.ReadStreamFrameHeader(preparedServer)
		if err != nil {
			return
		}
		var request workspacev0.MaterializeWorkspaceRequest
		if err := frameio.ReadProtoFrame(preparedServer, &request); err != nil {
			return
		}
		artifactHeader, artifactSize, err := wire.ReadStreamFrameHeader(preparedServer)
		if err != nil || artifactHeader.Type != wire.StreamTypeWorkspaceArtifact {
			return
		}
		_, _ = io.Copy(io.Discard, &io.LimitedReader{R: preparedServer, N: int64(artifactSize)})
		var buf [1]byte
		_, _ = preparedServer.Read(buf[:])
	}()
	client := &workspaceMaterializerTestClient{}
	session := &workspaceMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	pool := workspacePreparedRuntimePool(t, workspaceMount, session)
	materializer := WorkspaceMaterializer{
		CAS:            store,
		TempDir:        t.TempDir(),
		Heartbeat:      time.Hour,
		StartupTimeout: time.Millisecond,
		RuntimePool:    pool,
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
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedWorkspaceMount(t, preparedServer, workspaceMount, workspaceMount.RuntimeInstanceID)
	client := &workspaceMaterializerTestClient{
		renewErrors: []error{errors.New("renew failed")},
	}
	session := &workspaceMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	pool := workspacePreparedRuntimePool(t, workspaceMount, session)
	materializer := WorkspaceMaterializer{
		CAS:         store,
		TempDir:     t.TempDir(),
		Heartbeat:   10 * time.Millisecond,
		PollEvery:   time.Hour,
		RuntimePool: pool,
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

func TestRunWorkspaceMountPropagatesCloseFailureAndRetainsPreparedRuntimeCheckout(t *testing.T) {
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-close-prepared"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	target := runtimeCapacityTarget(workspaceMount.RuntimeInstanceID, workspaceMount.RuntimeEpoch)
	closeFailure := errors.New("prepared runtime cleanup failed")
	session := &workspaceMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
		closeErr:  closeFailure,
	}
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	key := runtimeInstanceIDFromWorkspaceMount(workspaceMount)
	ready := newPreparedRuntimeSignal()
	ready.finish(nil)
	pool.entries[key] = []preparedRuntimeEntry{{
		session: session, poolKey: key, runtimeInstanceID: target.ID,
		runtimeEpoch: target.WorkerEpoch, target: target,
		exit: newPreparedRuntimeSignal(), ready: ready,
	}}
	go acknowledgePreparedWorkspaceMount(t, preparedServer, workspaceMount, key)
	client := &workspaceMaterializerTestClient{
		renewErrors: []error{errors.New("renew failed")},
	}
	materializer := WorkspaceMaterializer{
		CAS:         store,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Millisecond,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}

	err := materializer.RunWorkspaceMount(context.Background(), workspaceMount, client)
	if !errors.Is(err, closeFailure) {
		t.Fatalf("materializer error = %v, want close failure", err)
	}
	if !pool.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatal("prepared runtime checkout was released after close failure")
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 1 {
		t.Fatalf("capacity reservations after close failure = %d, want 1", got)
	}
}

func TestWorkspaceMaterializerFailsWorkspaceMountWhenSessionExits(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	exit := make(chan error, 1)
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		acknowledgePreparedWorkspaceMount(t, preparedServer, workspaceMount, workspaceMount.RuntimeInstanceID)
		exit <- errors.New("the Firecracker exited")
	}()
	client := &workspaceMaterializerTestClient{}
	session := &workspaceMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
		exit:      exit,
	}
	pool := workspacePreparedRuntimePool(t, workspaceMount, session)
	materializer := WorkspaceMaterializer{
		CAS:         store,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Hour,
		PollEvery:   time.Hour,
		RuntimePool: pool,
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

func TestWorkspaceMaterializerRegistersPreparedRuntimeOverOpenedStream(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
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
		streams: []io.ReadWriteCloser{preparedClient},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		acknowledgePreparedWorkspaceMount(t, preparedServer, workspaceMount, "runtime-key")
	}()

	err := (WorkspaceMaterializer{}).registerWorkspaceMount(ctx, session, workspaceMount, workspacePath, "runtime-key")
	if err != nil {
		t.Fatal(err)
	}
	<-done
	opened := session.openedStreams()
	if len(opened) != 1 || opened[0] != preparedClient {
		t.Fatalf("opened streams = %+v, want prepared runtime workspaceMount over OpenStream", opened)
	}
}

func acknowledgePreparedWorkspaceMount(t *testing.T, stream io.ReadWriteCloser, workspaceMount workerapi.WorkspaceMount, runtimeKey string) {
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
	if !request.UsePreparedRuntime || request.RuntimeInstanceId != runtimeKey {
		t.Errorf("prepared runtime request use=%v runtime_instance_id=%q", request.UsePreparedRuntime, request.RuntimeInstanceId)
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

type workspaceMaterializerTestSession struct {
	mu        sync.Mutex
	operation io.ReadWriteCloser
	streams   []io.ReadWriteCloser
	opened    []io.ReadWriteCloser
	exit      <-chan error
	closeErr  error
	closed    int
}

func (s *workspaceMaterializerTestSession) Stream() vm.Stream {
	return nil
}

func (s *workspaceMaterializerTestSession) OpenStream(context.Context) (vm.Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed > 0 {
		return nil, errors.New("test session is closed")
	}
	if len(s.streams) > 0 {
		stream := s.streams[0]
		s.streams = s.streams[1:]
		s.opened = append(s.opened, stream)
		return testVMStream(stream), nil
	}
	s.opened = append(s.opened, s.operation)
	return testVMStream(s.operation), nil
}

func (s *workspaceMaterializerTestSession) Close(context.Context) error {
	s.mu.Lock()
	s.closed++
	operation := s.operation
	opened := append([]io.ReadWriteCloser(nil), s.opened...)
	streams := append([]io.ReadWriteCloser(nil), s.streams...)
	closeErr := s.closeErr
	s.mu.Unlock()
	if operation != nil {
		_ = operation.Close()
	}
	for _, stream := range opened {
		if stream != nil {
			_ = stream.Close()
		}
	}
	for _, stream := range streams {
		_ = stream.Close()
	}
	return closeErr
}

func (s *workspaceMaterializerTestSession) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *workspaceMaterializerTestSession) openedStreams() []io.ReadWriteCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]io.ReadWriteCloser(nil), s.opened...)
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
	cancel          context.CancelFunc
	workspaceExec   *workerapi.WorkspaceExec
	execClaims      []workerapi.WorkspaceExecClaimRequest
	execCompletions []workerapi.WorkspaceExecCompleteRequest
	completeErrors  []error
	renewErrors     []error
	renews          []workerapi.WorkspaceMountRenewRequest
	mounted         []workerapi.WorkspaceMountMountedRequest
	onMounted       func()
	stops           int
	captures        []workerapi.WorkspaceMountCaptureRequest
	failures        []workerapi.WorkspaceMountFailRequest
}

func (c *workspaceMaterializerTestClient) RenewWorkspaceMount(_ context.Context, request workerapi.WorkspaceMountRenewRequest) (workerapi.WorkspaceMountResponse, error) {
	c.renews = append(c.renews, request)
	if len(c.renewErrors) > 0 {
		err := c.renewErrors[0]
		c.renewErrors = c.renewErrors[1:]
		return workerapi.WorkspaceMountResponse{}, err
	}
	return workerapi.WorkspaceMountResponse{State: "mounting"}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspaceMountMounted(_ context.Context, request workerapi.WorkspaceMountMountedRequest) (workerapi.WorkspaceMountResponse, error) {
	c.mounted = append(c.mounted, request)
	if c.onMounted != nil {
		c.onMounted()
	}
	return workerapi.WorkspaceMountResponse{State: "mounted"}, nil
}

func (c *workspaceMaterializerTestClient) CaptureWorkspaceMount(_ context.Context, request workerapi.WorkspaceMountCaptureRequest) (workerapi.WorkspaceMountCaptureResponse, error) {
	c.captures = append(c.captures, request)
	return workerapi.WorkspaceMountCaptureResponse{VersionID: "version-1"}, nil
}

func (c *workspaceMaterializerTestClient) StopWorkspaceMount(context.Context, workerapi.WorkspaceMountStopRequest) (workerapi.WorkspaceMountResponse, error) {
	c.stops++
	return workerapi.WorkspaceMountResponse{State: "unmounted"}, nil
}

func (c *workspaceMaterializerTestClient) FailWorkspaceMount(_ context.Context, request workerapi.WorkspaceMountFailRequest) (workerapi.WorkspaceMountResponse, error) {
	c.failures = append(c.failures, request)
	return workerapi.WorkspaceMountResponse{State: "failed"}, nil
}

func (c *workspaceMaterializerTestClient) ClaimWorkspaceExec(_ context.Context, request workerapi.WorkspaceExecClaimRequest) (workerapi.WorkspaceExecClaimResponse, error) {
	c.execClaims = append(c.execClaims, request)
	return workerapi.WorkspaceExecClaimResponse{Exec: c.workspaceExec}, nil
}

func (c *workspaceMaterializerTestClient) CompleteWorkspaceExec(_ context.Context, request workerapi.WorkspaceExecCompleteRequest) (workerapi.WorkspaceMountResponse, error) {
	c.execCompletions = append(c.execCompletions, request)
	if len(c.completeErrors) > 0 {
		err := c.completeErrors[0]
		c.completeErrors = c.completeErrors[1:]
		return workerapi.WorkspaceMountResponse{}, err
	}
	if c.cancel != nil {
		c.cancel()
	}
	return workerapi.WorkspaceMountResponse{
		State:             "unmounting",
		FinalizationKind:  "capture",
		FencingGeneration: request.FencingGeneration,
	}, nil
}

type workspaceMaterializerHTTPError int

func (e workspaceMaterializerHTTPError) Error() string {
	return http.StatusText(int(e))
}

func (e workspaceMaterializerHTTPError) HTTPStatusCode() int {
	return int(e)
}

type discardReadWriteCloser struct{}

func (discardReadWriteCloser) Read([]byte) (int, error)    { return 0, io.EOF }
func (discardReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardReadWriteCloser) Close() error                { return nil }
