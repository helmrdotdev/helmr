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

func TestWorkspaceMaterializerRenewsWhileAwaitingPreparedRuntime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			store, mount := testWorkspaceMountArtifacts(t)
			mount.ID, mount.OrgID = "mount-await-ready", "org-1"
			pool := workspacePreparedRuntimePool(t, mount, &workspaceMaterializerTestSession{})
			key := runtimeInstanceIDFromWorkspaceMount(mount)
			pool.entries[key][0].ready = newPreparedRuntimeSignal()
			renewed := make(chan struct{})
			client := &workspaceMaterializerTestClient{renewed: renewed}
			if fail {
				client.renewErrors = []error{errors.New("renew failed")}
			}
			materializer := WorkspaceMaterializer{CAS: store, Heartbeat: time.Millisecond, RuntimePool: pool}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- materializer.RunWorkspaceMount(ctx, mount, client) }()
			if !fail {
				select {
				case <-renewed:
				case <-time.After(time.Second):
					t.Fatal("pending runtime admission did not renew")
				}
				cancel()
			}
			select {
			case err := <-done:
				if fail && (err == nil || !strings.Contains(err.Error(), "renew workspace mount")) {
					t.Fatalf("renew failure=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("pending runtime admission did not cancel")
			}
		})
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
	_, err = store.Put(context.Background(), workspaceArtifact.MediaType, file)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return store, workerapi.WorkspaceMount{
		RuntimeInstanceID: "runtime-instance-1",
		RuntimeEpoch:      1,
		RuntimeIdentityID: "runtime-1",
		WorkspaceImage: workerapi.CASObject{
			Digest: imageObject.Digest, SizeBytes: imageObject.SizeBytes, MediaType: imageObject.MediaType,
		},
		RootfsDigest: "sha256:runtime-rootfs",
		Target: workerapi.ComputerMountTarget{
			BaseWorkspaceVersionID: "version-1",
		},
		WorkspaceMountPath: "/workspace",
	}
}

func workspacePreparedRuntimePool(t *testing.T, mount workerapi.WorkspaceMount, session vm.Session) *PreparedRuntimePool {
	t.Helper()
	target := runtimeCapacityTarget(mount.RuntimeInstanceID, mount.RuntimeEpoch)
	target.Source.WorkspaceID = mount.WorkspaceID
	target.Source.Computer = &workerapi.RuntimeComputerSource{VersionID: mount.Target.BaseWorkspaceVersionID}
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

	session, runtimeInstanceID, err := materializer.materializeSession(context.Background(), &workspaceMount)
	if err != nil {
		t.Fatal(err)
	}
	if session != wantSession {
		t.Fatalf("session = %T %p, want %T %p", session, session, wantSession, wantSession)
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
	if len(store.getCalls) != 0 {
		t.Fatalf("prepared computer unexpectedly read CAS: %+v", store.getCalls)
	}
	if err := pool.ReleaseCheckout(workspaceMount.RuntimeInstanceID, workspaceMount.RuntimeEpoch); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceMaterializerReleasesCheckoutOnRestoreProvenanceFailure(t *testing.T) {
	tests := []struct {
		name                 string
		mountCheckpointID    string
		mountSourceVersionID string
		wantCode             string
	}{
		{
			name: "checkpoint mismatch", mountCheckpointID: "checkpoint-other",
			mountSourceVersionID: "version-b", wantCode: "workspace_restore_checkpoint_mismatch",
		},
		{
			name: "missing source version", mountCheckpointID: "checkpoint-b",
			wantCode: "workspace_restore_source_invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, mount := testWorkspaceMountArtifacts(t)
			mount.RestoreCheckpointID = test.mountCheckpointID
			mount.RestoreSourceVersionID = test.mountSourceVersionID
			session := &workspaceMaterializerTestSession{}
			pool := workspacePreparedRuntimePool(t, mount, session)
			key := runtimeInstanceIDFromWorkspaceMount(mount)
			pool.entries[key][0].target.Source.Restore = &workerapi.RuntimeRestore{
				CheckpointID: "checkpoint-b", RunID: "run-b", AttemptNumber: 1,
			}
			materializer := WorkspaceMaterializer{CAS: store, RuntimePool: pool}

			_, _, err := materializer.materializeSession(context.Background(), &mount)
			var failure workspaceMountFailure
			if !errors.As(err, &failure) || failure.code != test.wantCode {
				t.Fatalf("materialize error = %v, want %s", err, test.wantCode)
			}
			if session.closeCount() != 1 {
				t.Fatalf("session close count = %d, want 1", session.closeCount())
			}
			if pool.runtimeCheckedOut(mount.RuntimeInstanceID, mount.RuntimeEpoch) {
				t.Fatal("failed restore provenance retained runtime checkout")
			}
			if got := len(pool.Capacity.Snapshot().Reservations); got != 0 {
				t.Fatalf("capacity reservations = %d, want 0", got)
			}
		})
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
	if len(store.getCalls) != 0 {
		t.Fatalf("prepared computer unexpectedly read CAS: %+v", store.getCalls)
	}
}

func TestWorkspaceMaterializerPreparedComputerSkipsWorkspaceCAS(t *testing.T) {
	store, mount := testWorkspaceMountArtifacts(t)
	mount.Target = workerapi.ComputerMountTarget{
		BaseWorkspaceVersionID: mount.Target.BaseWorkspaceVersionID,
	}
	session := &workspaceMaterializerTestSession{}
	pool := workspacePreparedRuntimePool(t, mount, session)
	materializer := WorkspaceMaterializer{
		CAS:         store,
		RuntimePool: pool,
	}

	gotSession, _, err := materializer.materializeSession(context.Background(), &mount)
	if err != nil {
		t.Fatal(err)
	}
	if gotSession != session {
		t.Fatalf("session = %v, want prepared session", gotSession)
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
		BaseWorkspaceVersionID: "version-2",
		ProcessID:              "process-1",
		WorkspaceID:            "workspace-1",
		WorkspaceMountID:       "mount-1",
		RequestFingerprint:     strings.Repeat("a", 64),
		Request:                json.RawMessage(`{"command":["sh","-c","printf ok"],"cwd":"/workspace","env":{},"timeout_ms":1000}`),
		Stdin:                  []byte("input"),
		Secrets:                []workerapi.SecretDelivery{{Env: &workerapi.SecretEnv{Name: "TOKEN"}, Value: secretValue}},
		WorkspaceLeaseID:       "lease-1",
		WriteCapability:        "capability-1",
		FencingGeneration:      2,
		OwnershipGeneration:    3,
		WriterGeneration:       4,
		ExpiresAt:              time.Now().Add(time.Minute),
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
		if request.GetBaseWorkspaceVersionId() != exec.BaseWorkspaceVersionID || request.GetOwnershipGeneration() != exec.OwnershipGeneration || request.GetWriterGeneration() != exec.WriterGeneration ||
			request.GetEnvelope().GetChannelToken() != "channel-token" ||
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
			BaseWorkspaceVersionID: "version-2", ProcessID: "process-1", WorkspaceMountID: "mount-2",
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-close-prepared"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	target := runtimeCapacityTarget(workspaceMount.RuntimeInstanceID, workspaceMount.RuntimeEpoch)
	target.Source.WorkspaceID = workspaceMount.WorkspaceID
	target.Source.Computer = &workerapi.RuntimeComputerSource{VersionID: workspaceMount.Target.BaseWorkspaceVersionID}
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
		onMounted: cancel,
	}
	materializer := WorkspaceMaterializer{
		CAS:         store,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Hour,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}

	err := materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	if len(client.mounted) != 1 {
		t.Fatalf("mounted requests = %d, want 1", len(client.mounted))
	}
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

func TestWorkspaceMaterializerOwnsProgramStartFailureCleanup(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedWorkspaceMount(
		t,
		preparedServer,
		workspaceMount,
		workspaceMount.RuntimeInstanceID,
	)
	rawSession := &workspaceMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	pool := workspacePreparedRuntimePool(t, workspaceMount, rawSession)
	sessions := NewWorkspaceMountSessions()
	mounted := make(chan struct{})
	client := &workspaceMaterializerTestClient{onMounted: func() { close(mounted) }}
	materializer := WorkspaceMaterializer{
		CAS:         store,
		Sessions:    sessions,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Hour,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}
	result := make(chan error, 1)
	go func() {
		result <- materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	}()
	select {
	case <-mounted:
	case <-time.After(5 * time.Second):
		t.Fatal("Workspace Mount did not become ready")
	}
	if err := sessions.FailWorkspaceMountSession(ctx, workspaceMount.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "failed before start proof") {
			t.Fatalf("materializer error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Workspace Mount owner did not finish Program start failure")
	}
	if rawSession.closeCount() == 0 {
		t.Fatal("Workspace Mount VM was not closed")
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_mount_program_start_failed") ||
		strings.Contains(got, "exec image runtime") {
		t.Fatalf("failure error = %s", got)
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 0 {
		t.Fatalf("capacity reservations = %d, want 0", got)
	}
}

func TestWorkspaceMaterializerProgramStartFailureKeepsCapacityWhenRuntimeCloseFails(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-close-failed"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedWorkspaceMount(
		t,
		preparedServer,
		workspaceMount,
		workspaceMount.RuntimeInstanceID,
	)
	rawCause := "signed-url-secret-sentinel"
	rawSession := &workspaceMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
		closeErr:  errors.New(rawCause),
	}
	pool := workspacePreparedRuntimePool(t, workspaceMount, rawSession)
	sessions := NewWorkspaceMountSessions()
	mounted := make(chan struct{})
	client := &workspaceMaterializerTestClient{onMounted: func() { close(mounted) }}
	materializer := WorkspaceMaterializer{
		CAS:         store,
		Sessions:    sessions,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Hour,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}
	result := make(chan error, 1)
	go func() {
		result <- materializer.RunWorkspaceMount(ctx, workspaceMount, client)
	}()
	select {
	case <-mounted:
	case <-time.After(5 * time.Second):
		t.Fatal("Workspace Mount did not become ready")
	}
	if err := sessions.FailWorkspaceMountSession(ctx, workspaceMount.ID); err == nil ||
		!strings.Contains(err.Error(), rawCause) {
		t.Fatalf("failure request error = %v, want local cleanup cause", err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "workspace mount runtime cleanup failed") {
			t.Fatalf("materializer error = %v, want static cleanup failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Workspace Mount owner did not finish Program start failure")
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "workspace_mount_runtime_close_failed") ||
		strings.Contains(got, rawCause) {
		t.Fatalf("failure error = %s", got)
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 1 {
		t.Fatalf("capacity reservations = %d, want 1 until cleanup is proven", got)
	}
}

func TestWorkspaceMaterializerRegistersPreparedRuntimeOverOpenedStream(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	_, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-1"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	session := &workspaceMaterializerTestSession{
		streams: []io.ReadWriteCloser{preparedClient},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		acknowledgePreparedWorkspaceMount(t, preparedServer, workspaceMount, "runtime-key")
	}()

	err := (WorkspaceMaterializer{}).registerWorkspaceMount(ctx, session, workspaceMount, "runtime-key")
	if err != nil {
		t.Fatal(err)
	}
	<-done
	opened := session.openedStreams()
	if len(opened) != 1 || opened[0] != preparedClient {
		t.Fatalf("opened streams = %+v, want prepared runtime workspaceMount over OpenStream", opened)
	}
}

func TestWorkspaceMaterializerValidatesSuccessReceiptsOnlyAfterRunningState(t *testing.T) {
	_, workspaceMount := testWorkspaceMountArtifacts(t)
	workspaceMount.ID = "mat-receipt"
	workspaceMount.OrgID = "org-1"
	workspaceMount.WorkspaceID = "workspace-1"
	workspaceMount.GuestdChannelToken = "channel-token"
	workspaceMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))

	tests := []struct {
		name     string
		response func(*workspacev0.MaterializeWorkspaceRequest) *workspacev0.MaterializeWorkspaceResponse
		want     string
		notWant  string
	}{
		{
			name: "failed with phase error",
			response: func(*workspacev0.MaterializeWorkspaceRequest) *workspacev0.MaterializeWorkspaceResponse {
				return &workspacev0.MaterializeWorkspaceResponse{
					Status: "failed",
					Phases: []*workspacev0.WorkspaceMountPhase{{
						Name:  "guest_workspace_target_verify",
						Error: "workspace tree digest mismatch",
					}},
				}
			},
			want:    "guest_workspace_target_verify: workspace tree digest mismatch",
			notWant: "target does not match",
		},
		{
			name: "failed without phase error",
			response: func(*workspacev0.MaterializeWorkspaceRequest) *workspacev0.MaterializeWorkspaceResponse {
				return &workspacev0.MaterializeWorkspaceResponse{Status: "failed"}
			},
			want: `workspace materialize returned state "failed"`,
		},
		{
			name: "running without target",
			response: func(*workspacev0.MaterializeWorkspaceRequest) *workspacev0.MaterializeWorkspaceResponse {
				return &workspacev0.MaterializeWorkspaceResponse{Status: "running", GuestdChannelTokenHash: workspaceMount.GuestdChannelTokenHash}
			},
			want: "target does not match",
		},
		{
			name: "running without channel receipt",
			response: func(request *workspacev0.MaterializeWorkspaceRequest) *workspacev0.MaterializeWorkspaceResponse {
				return &workspacev0.MaterializeWorkspaceResponse{Status: "running", Target: request.Target}
			},
			want: "guest channel token hash mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preparedClient, preparedServer := net.Pipe()
			defer preparedServer.Close()
			go respondToPreparedWorkspaceMountWithRequest(t, preparedServer, test.response)
			err := (WorkspaceMaterializer{}).registerWorkspaceMount(context.Background(), &workspaceMaterializerTestSession{
				streams: []io.ReadWriteCloser{preparedClient},
			}, workspaceMount, "runtime-key")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("register error = %v, want %q", err, test.want)
			}
			if test.notWant != "" && strings.Contains(err.Error(), test.notWant) {
				t.Fatalf("register error = %v, do not want %q", err, test.notWant)
			}
		})
	}
}

func respondToPreparedWorkspaceMountWithRequest(t *testing.T, stream io.ReadWriteCloser, response func(*workspacev0.MaterializeWorkspaceRequest) *workspacev0.MaterializeWorkspaceResponse) {
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
	if err := frameio.WriteProtoFrame(stream, response(&request)); err != nil {
		t.Errorf("write materialize response: %v", err)
	}
}

func acknowledgePreparedWorkspaceMount(t *testing.T, stream io.ReadWriteCloser, workspaceMount workerapi.WorkspaceMount, runtimeKey string) {
	t.Helper()
	respondToPreparedWorkspaceMountWithRequest(t, stream, func(request *workspacev0.MaterializeWorkspaceRequest) *workspacev0.MaterializeWorkspaceResponse {
		if !request.UsePreparedRuntime || request.RuntimeInstanceId != runtimeKey {
			t.Errorf("prepared runtime request use=%v runtime_instance_id=%q", request.UsePreparedRuntime, request.RuntimeInstanceId)
		}
		return &workspacev0.MaterializeWorkspaceResponse{
			Status:                 "running",
			GuestdChannelTokenHash: workspaceMount.GuestdChannelTokenHash,
			Target:                 request.Target,
		}
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
	failErrors      []error
	cancel          context.CancelFunc
	workspaceExec   *workerapi.WorkspaceExec
	execClaims      []workerapi.WorkspaceExecClaimRequest
	execCompletions []workerapi.WorkspaceExecCompleteRequest
	completeErrors  []error
	renewErrors     []error
	renews          []workerapi.WorkspaceMountRenewRequest
	renewed         chan struct{}
	renewedOnce     sync.Once
	mounted         []workerapi.WorkspaceMountMountedRequest
	onMounted       func()
	stops           int
	captures        []workerapi.WorkspaceMountCaptureRequest
	failures        []workerapi.WorkspaceMountFailRequest
}

func (c *workspaceMaterializerTestClient) RenewWorkspaceMount(_ context.Context, request workerapi.WorkspaceMountRenewRequest) (workerapi.WorkspaceMountResponse, error) {
	c.renews = append(c.renews, request)
	if c.renewed != nil {
		c.renewedOnce.Do(func() { close(c.renewed) })
	}
	if len(c.renewErrors) > 0 {
		err := c.renewErrors[0]
		c.renewErrors = c.renewErrors[1:]
		return workerapi.WorkspaceMountResponse{}, err
	}
	return workerapi.WorkspaceMountResponse{Status: "mounting"}, nil
}

func (c *workspaceMaterializerTestClient) MarkWorkspaceMountMounted(_ context.Context, request workerapi.WorkspaceMountMountedRequest) (workerapi.WorkspaceMountResponse, error) {
	c.mounted = append(c.mounted, request)
	if c.onMounted != nil {
		c.onMounted()
	}
	return workerapi.WorkspaceMountResponse{Status: "mounted"}, nil
}

func (c *workspaceMaterializerTestClient) CaptureWorkspaceMount(_ context.Context, request workerapi.WorkspaceMountCaptureRequest) (workerapi.WorkspaceMountCaptureResponse, error) {
	c.captures = append(c.captures, request)
	return workerapi.WorkspaceMountCaptureResponse{VersionID: "version-1"}, nil
}

func (c *workspaceMaterializerTestClient) StopWorkspaceMount(context.Context, workerapi.WorkspaceMountStopRequest) (workerapi.WorkspaceMountResponse, error) {
	c.stops++
	return workerapi.WorkspaceMountResponse{Status: "unmounted"}, nil
}

func (c *workspaceMaterializerTestClient) FailWorkspaceMount(_ context.Context, request workerapi.WorkspaceMountFailRequest) (workerapi.WorkspaceMountResponse, error) {
	c.failures = append(c.failures, request)
	if len(c.failErrors) > 0 {
		err := c.failErrors[0]
		c.failErrors = c.failErrors[1:]
		return workerapi.WorkspaceMountResponse{}, err
	}
	return workerapi.WorkspaceMountResponse{Status: "failed"}, nil
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
		Status:            "unmounting",
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

func TestArtifactCachePinSurvivesReplacementAndEviction(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "artifact")
	oldBytes := []byte("old-corrupt")
	if err := os.WriteFile(public, oldBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	var pin string
	var source *os.File
	var cleanup func()
	if err := localcache.WithRootLock(root, func(localcache.RootLock) error {
		var err error
		pin, source, cleanup, err = pinCachedArtifact(t.TempDir(), "pin", public)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	newer := filepath.Join(root, ".replacement")
	newBytes := []byte("correct replacement")
	if err := os.WriteFile(newer, newBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := localcache.WithRootLock(root, func(localcache.RootLock) error { return os.Rename(newer, public) }); err != nil {
		t.Fatal(err)
	}
	if err := validateCachedArtifact(pin, workerapi.CASObject{Digest: sha256sum.DigestBytes(newBytes), SizeBytes: int64(len(newBytes))}); err == nil {
		t.Fatal("invalid pinned inode accepted")
	}
	cleanup()
	body, err := os.ReadFile(public)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, newBytes) {
		t.Fatal("pin cleanup changed replacement")
	}
	if err := localcache.WithRootLock(root, func(localcache.RootLock) error {
		var err error
		pin, source, cleanup, err = pinCachedArtifact(t.TempDir(), "pin", public)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := localcache.EnforceByteLimit(root, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(public); !os.IsNotExist(err) {
		t.Fatalf("public entry not evicted: %v", err)
	}
	if err := finishCachedArtifact(pin, source, int64(len(newBytes))); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(pin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, newBytes) {
		t.Fatal("eviction changed private artifact")
	}
}

func TestArtifactCacheConcurrentRestoreAndEviction(t *testing.T) {
	store, mount := testWorkspaceMountArtifacts(t)
	cache, temp := t.TempDir(), t.TempDir()
	materializer := WorkspaceMaterializer{CAS: store, ArtifactCacheDir: cache}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Go(func() {
			for j := 0; j < 10; j++ {
				path, cleanup, err := materializer.restoreCASObject(t.Context(), temp, "image", mount.WorkspaceImage)
				if err != nil {
					t.Error(err)
					return
				}
				content, readErr := os.ReadFile(path)
				cleanup()
				if readErr != nil {
					t.Error(readErr)
					return
				}
				if string(content) != "oci image" {
					t.Errorf("wrong content: %q", content)
					return
				}
				if _, err := localcache.EnforceByteLimit(filepath.Join(cache, "sha256"), 1, nil); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	group.Wait()
	pins, err := filepath.Glob(filepath.Join(cache, "sha256", ".pinned-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 0 {
		t.Fatalf("leaked pins: %v", pins)
	}
}

func TestArtifactCacheFallbackCopiesPinnedFD(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "cache")
	if err := os.WriteFile(public, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(public)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := os.Remove(public); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(public, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "private")
	if err := finishCachedArtifact(target, source, 3); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "old" {
		t.Fatalf("copied replacement: %q", body)
	}
	if _, err := source.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("source FD not closed: %v", err)
	}
	source, err = os.Open(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishCachedArtifact(target, source, 3); err == nil {
		t.Fatal("existing private target overwritten")
	}
	if _, err := source.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("source FD leaked on failure: %v", err)
	}
}

func TestArtifactCacheFallbackRejectsSizeBeforeCopy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	target := filepath.Join(root, "private")
	if err := os.WriteFile(path, []byte("oversized"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishCachedArtifact(target, source, 1); err == nil {
		t.Fatal("size mismatch accepted")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("oversized source was copied: %v", err)
	}
	if _, err := source.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("source FD leaked: %v", err)
	}
}

func TestCheckpointReleaseFailureReportsWithoutVMExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, server := net.Pipe()
	defer server.Close()
	store, mount := testWorkspaceMountArtifacts(t)
	mount.ID, mount.OrgID, mount.WorkspaceID = "release-failure", "org", "workspace"
	mount.GuestdChannelToken = "channel-token"
	mount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedWorkspaceMount(t, server, mount, mount.RuntimeInstanceID)
	stopErr, reportErr := errors.New("VM stop unproved"), errors.New("failure acknowledgement lost")
	raw := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{conn}, operation: discardReadWriteCloser{}, closeErr: stopErr}
	pool := workspacePreparedRuntimePool(t, mount, raw)
	sessions := NewWorkspaceMountSessions()
	mounted := make(chan struct{})
	client := &workspaceMaterializerTestClient{onMounted: func() { close(mounted) }, failErrors: []error{reportErr}}
	materializer := WorkspaceMaterializer{CAS: store, Sessions: sessions, TempDir: t.TempDir(), Heartbeat: time.Hour, PollEvery: time.Hour, RuntimePool: pool}
	done := make(chan error, 1)
	go func() { done <- materializer.RunWorkspaceMount(ctx, mount, client) }()
	select {
	case <-mounted:
	case <-ctx.Done():
		t.Fatal("mount not ready")
	}
	borrowed, err := sessions.OpenWorkspaceMountSession(ctx, mount.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Session.Close(context.Background())
	if err := borrowed.Session.(CheckpointSourceReleaser).ReleaseCheckpointSource(ctx); !errors.Is(err, stopErr) {
		t.Fatalf("release: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, stopErr) || !errors.Is(err, reportErr) {
			t.Fatalf("lost stop/report failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("mount waited for VM exit after failed stop")
	}
	if len(client.failures) != 2 {
		t.Fatalf("failure reporting attempts = %d", len(client.failures))
	}
	if !pool.runtimeCheckedOut(mount.RuntimeInstanceID, mount.RuntimeEpoch) {
		t.Fatal("released unproven runtime")
	}
	if len(pool.Capacity.Snapshot().Reservations) != 1 {
		t.Fatal("released capacity before physical reclaim")
	}
	if raw.closeCount() != 1 {
		t.Fatal("retried cached session close instead of deferring physical reclaim")
	}
	// Reconciliation receives a CP-authorized target after active leases expire.
	// It must use host cleanup, not retry the cached session Close failure.
	connector := &cleanupRuntimeConnector{err: errors.New("process still alive")}
	pool.Connector = connector
	target := runtimeCapacityTarget(mount.RuntimeInstanceID, mount.RuntimeEpoch)
	target.Source.WorkspaceID = mount.WorkspaceID
	target.Source.Computer = &workerapi.RuntimeComputerSource{VersionID: mount.Target.BaseWorkspaceVersionID}
	control := &typedRuntimeClient{}
	if err := pool.ReclaimFailedRuntimeTarget(ctx, control, target); err == nil {
		t.Fatal("unproved host cleanup succeeded")
	}
	if !pool.runtimeCheckedOut(mount.RuntimeInstanceID, mount.RuntimeEpoch) || len(control.failed) != 0 {
		t.Fatal("released or published proof before physical cleanup")
	}
	connector.err = nil
	if err := pool.ReclaimFailedRuntimeTarget(ctx, control, target); err != nil {
		t.Fatal(err)
	}
	if pool.runtimeCheckedOut(mount.RuntimeInstanceID, mount.RuntimeEpoch) || len(pool.Capacity.Snapshot().Reservations) != 0 {
		t.Fatal("retained runtime after proved cleanup")
	}
	if len(control.failed) != 1 || control.failed[0].CleanupProof == nil || raw.closeCount() != 1 {
		t.Fatal("reclaim did not publish host proof independently of cached Close")
	}
}

func TestPreparedComputerCheckoutRejectsChangedIdentityWithoutConsuming(t *testing.T) {
	_, mount := testWorkspaceMountArtifacts(t)
	mount.WorkspaceID = "computer-1"
	session := &workspaceMaterializerTestSession{}
	pool := workspacePreparedRuntimePool(t, mount, session)
	for _, change := range []func(*workerapi.WorkspaceMount){func(m *workerapi.WorkspaceMount) { m.WorkspaceID = "computer-2" }, func(m *workerapi.WorkspaceMount) { m.Target.BaseWorkspaceVersionID = "other-version" }} {
		wrong := mount
		change(&wrong)
		if _, _, ok := pool.Checkout(t.Context(), wrong); ok {
			t.Fatal("changed Computer source accepted")
		}
	}
	if got, _, ok := pool.Checkout(t.Context(), mount); !ok || got != session {
		t.Fatal("valid source was consumed by mismatch")
	}
}

func (*workspaceMaterializerTestClient) RegisterExecComputerObject(context.Context, workerapi.ExecComputerObjectRequest) error {
	return nil
}

func (*workspaceMaterializerTestClient) CertifyExecComputerObject(context.Context, workerapi.ExecComputerObjectRequest) error {
	return nil
}

func (*workspaceMaterializerTestClient) ReuseExecComputerObject(context.Context, workerapi.ExecComputerObjectRequest) error {
	return nil
}
