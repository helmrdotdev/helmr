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
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/localcache"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestComputerMaterializerUsesStartupTimeout(t *testing.T) {
	if got := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).startupTimeout(); got != computerStartupTimeout {
		t.Fatalf("startup timeout = %s, want %s", got, computerStartupTimeout)
	}
	custom := time.Second
	if got := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, StartupTimeout: custom}).startupTimeout(); got != custom {
		t.Fatalf("custom startup timeout = %s, want %s", got, custom)
	}
}

func TestComputerMaterializerRenewsWhileAwaitingPreparedRuntime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			store, mount := testComputerMountArtifacts(t)
			mount.ComputerInstanceID, mount.OrgID = "mount-await-ready", "org-1"
			pool := computerPreparedRuntimePool(t, mount, &computerMaterializerTestSession{})
			key := computerInstanceIDFromComputerMount(mount)
			pool.entries[key][0].ready = newPreparedRuntimeSignal()
			renewed := make(chan struct{})
			client := &computerMaterializerTestClient{renewed: renewed}
			if fail {
				client.renewErrors = []error{errors.New("renew failed")}
			}
			materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, Heartbeat: time.Millisecond, RuntimePool: pool}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- materializer.RunComputerMount(ctx, mount, client) }()
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
				if fail && (err == nil || !strings.Contains(err.Error(), "renew computer mount")) {
					t.Fatalf("renew failure=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("pending runtime admission did not cancel")
			}
		})
	}
}

func testComputerMountArtifacts(t *testing.T) (*fakeCAS, workerapi.ComputerInstanceAssignment) {
	t.Helper()
	store := &fakeCAS{objects: map[string][]byte{}}
	imageObject, err := store.Put(context.Background(), bundle.ComputerImageMediaType, strings.NewReader("oci image"))
	if err != nil {
		t.Fatal(err)
	}
	computerArtifact, cleanup, err := disk.CreateEmptyComputerArtifact(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	file, err := os.Open(computerArtifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Put(context.Background(), computerArtifact.MediaType, file)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return store, workerapi.ComputerInstanceAssignment{
		WriterGeneration:   2,
		DesiredVersion:     1,
		ObservedVersion:    1,
		ComputerInstanceID: uuid.NewV7().String(),
		EnvironmentID:      uuid.NewV7().String(),
		ComputerID:         uuid.NewV7().String(),
		RuntimeEpoch:       1,
		VMPlatformID:       "runtime-1",
		ComputerImage: workerapi.CASObject{
			Digest: imageObject.Digest, SizeBytes: imageObject.SizeBytes, MediaType: imageObject.MediaType,
		},
		RootfsDigest: "sha256:runtime-rootfs",
		Target: workerapi.ComputerMountTarget{
			BaseComputerDiskVersionID: "version-1",
		},
		ComputerMountPath: "/workspace",
	}
}

func computerPreparedRuntimePool(t *testing.T, mount workerapi.ComputerInstanceAssignment, session vm.Machine) *PreparedRuntimePool {
	t.Helper()
	target := runtimeReservationTarget(mount.ComputerInstanceID, mount.RuntimeEpoch)
	target.Source.ComputerID = mount.ComputerID
	target.Source.WriterGeneration = mount.WriterGeneration
	target.Source.Computer = &workerapi.RuntimeComputerSource{VersionID: mount.Target.BaseComputerDiskVersionID}
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.Reservations = newPreparedRuntimeReservations(t, 1)
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	key := computerInstanceIDFromComputerMount(mount)
	ready := newPreparedRuntimeSignal()
	ready.finish(nil)
	pool.entries[key] = []preparedRuntimeEntry{{
		session: session, poolKey: key, computerInstanceID: target.ID,
		runtimeEpoch: target.WorkerEpoch, target: target,
		exit: newPreparedRuntimeSignal(), ready: ready,
	}}
	return pool
}

func TestComputerMaterializerRestoreCASObjectUsesLocalCache(t *testing.T) {
	store, computerMount := testComputerMountArtifacts(t)
	cacheDir := t.TempDir()
	tempDir := t.TempDir()
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:              store,
		ArtifactCacheDir: cacheDir,
	}

	_, firstCleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "computer-image", computerMount.ComputerImage)
	if err != nil {
		t.Fatal(err)
	}
	firstCleanup()
	secondPath, secondCleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "computer-image", computerMount.ComputerImage)
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
	if got := store.getCalls[computerMount.ComputerImage.Digest]; got != 1 {
		t.Fatalf("CAS Get calls = %d, want 1", got)
	}
}

func TestComputerMaterializerRestoreCASObjectRefreshesInvalidLocalCache(t *testing.T) {
	store, computerMount := testComputerMountArtifacts(t)
	cacheDir := t.TempDir()
	tempDir := t.TempDir()
	cachePath, err := artifactCachePath(cacheDir, computerMount.ComputerImage.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("bad image"), 0o644); err != nil {
		t.Fatal(err)
	}
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:              store,
		ArtifactCacheDir: cacheDir,
	}

	path, cleanup, err := materializer.restoreCASObject(context.Background(), tempDir, "computer-image", computerMount.ComputerImage)
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
	if got := store.getCalls[computerMount.ComputerImage.Digest]; got != 1 {
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

func TestComputerMaterializerChecksOutPreparedRuntime(t *testing.T) {
	store, computerMount := testComputerMountArtifacts(t)
	wantSession := &computerMaterializerTestSession{}
	pool := computerPreparedRuntimePool(t, computerMount, wantSession)
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:         store,
		TempDir:     t.TempDir(),
		RuntimePool: pool,
	}

	session, computerInstanceID, err := materializer.materializeSession(context.Background(), &computerMount)
	if err != nil {
		t.Fatal(err)
	}
	if session != wantSession {
		t.Fatalf("session = %T %p, want %T %p", session, session, wantSession, wantSession)
	}
	if computerInstanceID != computerMount.ComputerInstanceID {
		t.Fatalf("runtime instance id = %q, want %q", computerInstanceID, computerMount.ComputerInstanceID)
	}
	if !pool.runtimeCheckedOut(computerMount.ComputerInstanceID, computerMount.RuntimeEpoch) {
		t.Fatal("prepared runtime was not checked out")
	}
	if got := store.getCalls[computerMount.ComputerImage.Digest]; got != 0 {
		t.Fatalf("computer image CAS gets = %d, want 0", got)
	}
	if len(store.getCalls) != 0 {
		t.Fatalf("prepared computer unexpectedly read CAS: %+v", store.getCalls)
	}
	if err := pool.ReleaseCheckout(computerMount.ComputerInstanceID, computerMount.RuntimeEpoch); err != nil {
		t.Fatal(err)
	}
}

func TestComputerMaterializerReleasesCheckoutOnRestoreProvenanceFailure(t *testing.T) {
	tests := []struct {
		name              string
		mountCheckpointID string
		wantCode          string
	}{
		{
			name: "checkpoint mismatch", mountCheckpointID: "checkpoint-other",
			wantCode: "computer_restore_checkpoint_mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, mount := testComputerMountArtifacts(t)
			mount.RestoreCheckpointID = test.mountCheckpointID
			session := &computerMaterializerTestSession{}
			pool := computerPreparedRuntimePool(t, mount, session)
			key := computerInstanceIDFromComputerMount(mount)
			pool.entries[key][0].target.Source.Restore = &workerapi.RuntimeRestore{
				CheckpointID: "checkpoint-b",
			}
			materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, RuntimePool: pool}

			_, _, err := materializer.materializeSession(context.Background(), &mount)
			var failure computerMountFailure
			if !errors.As(err, &failure) || failure.code != test.wantCode {
				t.Fatalf("materialize error = %v, want %s", err, test.wantCode)
			}
			if session.closeCount() != 1 {
				t.Fatalf("session close count = %d, want 1", session.closeCount())
			}
			if pool.runtimeCheckedOut(mount.ComputerInstanceID, mount.RuntimeEpoch) {
				t.Fatal("failed restore provenance retained runtime checkout")
			}
			if got := len(pool.Reservations.Snapshot().Reservations); got != 0 {
				t.Fatalf("capacity reservations = %d, want 0", got)
			}
		})
	}
}

func TestComputerMountPhaseErrorUsesLatestGuestError(t *testing.T) {
	got := computerMountPhaseError([]*computerv0.ComputerMountPhase{
		{Name: "guest_computer_image_restore"},
		{Name: "guest_computer_artifact_restore", Error: "extract computer artifact: permission denied"},
	})
	if got != "guest_computer_artifact_restore: extract computer artifact: permission denied" {
		t.Fatalf("phase error = %q", got)
	}
}

func TestComputerMaterializerFailsWhenPreparedRuntimeIsMissing(t *testing.T) {
	store, computerMount := testComputerMountArtifacts(t)
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:         store,
		RuntimePool: NewPreparedRuntimePool(nil, nil, 1, nil),
	}
	client := &computerMaterializerTestClient{}

	err := materializer.RunComputerMount(context.Background(), computerMount, client)
	if err == nil {
		t.Fatal("missing prepared runtime was accepted")
	}
	var failure computerMountFailure
	if !errors.As(err, &failure) || failure.code != "computer_runtime_not_prepared" {
		t.Fatalf("error = %v, want computer_runtime_not_prepared", err)
	}
	if len(client.failures) != 1 {
		t.Fatalf("computer mount failures = %d, want 1", len(client.failures))
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(client.failures[0].Error, &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "computer_runtime_not_prepared" {
		t.Fatalf("computer mount failure code = %q, want computer_runtime_not_prepared", body.Code)
	}
	if got := store.getCalls[computerMount.ComputerImage.Digest]; got != 0 {
		t.Fatalf("computer image CAS gets = %d, want 0", got)
	}
	if len(store.getCalls) != 0 {
		t.Fatalf("prepared computer unexpectedly read CAS: %+v", store.getCalls)
	}
}

func TestComputerMaterializerPreparedComputerSkipsComputerCAS(t *testing.T) {
	store, mount := testComputerMountArtifacts(t)
	mount.Target = workerapi.ComputerMountTarget{
		BaseComputerDiskVersionID: mount.Target.BaseComputerDiskVersionID,
	}
	session := &computerMaterializerTestSession{}
	pool := computerPreparedRuntimePool(t, mount, session)
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
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
	if got := store.getCalls[mount.ComputerImage.Digest]; got != 0 {
		t.Fatalf("computer image CAS gets = %d, want 0", got)
	}
	if err := pool.ReleaseCheckout(mount.ComputerInstanceID, mount.RuntimeEpoch); err != nil {
		t.Fatal(err)
	}
}

func TestComputerMaterializerDispatchesBasicExec(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	session := &computerMaterializerTestSession{operation: clientStream}
	secretValue := []byte("secret-value")
	exec := workerapi.ComputerCommand{
		CommandID:          "process-1",
		ComputerID:         "computer-1",
		ComputerInstanceID: "instance-1",
		RequestFingerprint: strings.Repeat("a", 64),
		Request:            json.RawMessage(`{"command":["sh","-c","printf ok"],"cwd":"/workspace","env":{},"timeout_ms":1000}`),
		Stdin:              []byte("input"),
		Secrets:            []workerapi.SecretDelivery{{Env: &workerapi.SecretEnv{Name: "TOKEN"}, Value: secretValue}},
		WriterGeneration:   4,
		ExpiresAt:          time.Now().Add(time.Minute),
	}
	guestDone := make(chan error, 1)
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestDone <- err
			return
		}
		if header.Type != wire.StreamTypeComputerBasicExec ||
			header.OperationID != exec.CommandID {
			guestDone <- fmt.Errorf("unexpected header: %+v", header)
			return
		}
		var request computerv0.ComputerBasicExecRequest
		if err := frameio.ReadProtoFrame(guestStream, &request); err != nil {
			guestDone <- err
			return
		}
		if request.GetEnvelope().GetComputerInstanceId() != exec.ComputerInstanceID || request.GetEnvelope().GetWriterGeneration() != exec.WriterGeneration ||
			request.GetEnvelope().GetChannelToken() != "channel-token" ||
			string(request.GetStdin()) != "input" ||
			len(request.GetSecrets()) != 1 ||
			request.GetSecrets()[0].GetPlacementKind() != "env" ||
			request.GetSecrets()[0].GetPlacementTarget() != "TOKEN" ||
			string(request.GetSecrets()[0].GetValue()) != "secret-value" {
			guestDone <- fmt.Errorf("unexpected BasicExec request: %+v", &request)
			return
		}
		for _, stream := range []string{"stdout", "stderr"} {
			if err := frameio.WriteProtoFrame(guestStream, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Output{Output: &computerv0.CommandOutputChunk{Stream: stream, Content: []byte(stream), ObservedAtUnixNano: time.Now().UnixNano()}}}); err != nil {
				guestDone <- err
				return
			}
		}
		guestDone <- frameio.WriteProtoFrame(guestStream, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{
			ExitCode:           7,
			Outcome:            "exited",
			RequestFingerprint: exec.RequestFingerprint,
		}}})
	}()
	client := &computerMaterializerTestClient{}
	completion, err := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).dispatchComputerBasicExec(
		context.Background(),
		session,
		workerapi.ComputerInstanceAssignment{
			OrgID: "org-1", ComputerID: "computer-1", ComputerInstanceID: "instance-1", WriterGeneration: 4,
			GuestdChannelToken: "channel-token",
		},
		exec, client,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-guestDone; err != nil {
		t.Fatal(err)
	}
	if completion.CommandID != exec.CommandID ||
		completion.ComputerInstanceID != "instance-1" || completion.WriterGeneration != exec.WriterGeneration ||
		completion.ExitCode == nil ||
		*completion.ExitCode != 7 ||
		len(client.commandLogs) != 2 ||
		string(client.commandLogs[0].Content) != "stdout" ||
		string(client.commandLogs[1].Content) != "stderr" ||
		completion.Outcome != "exited" {
		t.Fatalf("completion = %+v", completion)
	}
	for _, value := range secretValue {
		if value != 0 {
			t.Fatal("Secret plaintext was not cleared after dispatch")
		}
	}
}

func TestComputerMaterializerRejectsMismatchedBasicExecClaim(t *testing.T) {
	_, err := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).dispatchComputerBasicExec(
		context.Background(),
		&computerMaterializerTestSession{},
		workerapi.ComputerInstanceAssignment{
			ComputerID:         "computer-1",
			GuestdChannelToken: "channel-token",
		},
		workerapi.ComputerCommand{
			CommandID: "process-1", ComputerInstanceID: "instance-2",
			ComputerID: "computer-1", RequestFingerprint: strings.Repeat("a", 64),
			WriterGeneration: 1,
			ExpiresAt:        time.Now().Add(time.Minute),
		}, &computerMaterializerTestClient{},
	)
	var protocolError *computerBasicExecProtocolError
	if !errors.As(err, &protocolError) {
		t.Fatalf("error = %v, want protocol error", err)
	}
}

func TestComputerMaterializerRejectsGuestAuthorityOutcomes(t *testing.T) {
	for _, outcome := range []string{
		"computer_command_fenced",
		"computer_command_expired",
		"computer_command_invalid",
		"computer_command_fingerprint_conflict",
		"computer_command_unavailable",
		"future_outcome",
		"",
	} {
		t.Run(outcome, func(t *testing.T) {
			var protocolError *computerBasicExecProtocolError
			if err := validateComputerBasicCommandOutcome(outcome); !errors.As(err, &protocolError) {
				t.Fatalf("error = %v, want protocol error", err)
			}
		})
	}
}

func TestComputerMaterializerCompletionStopsOnNonRetryableError(t *testing.T) {
	client := &computerMaterializerTestClient{
		completeErrors: []error{computerMaterializerHTTPError(http.StatusBadRequest)},
	}
	err := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CompleteErrorBackoff: time.Nanosecond,
	}).completeComputerBasicExec(
		context.Background(),
		client,
		workerapi.ComputerCommandCompleteRequest{},
	)
	if err == nil {
		t.Fatal("non-retryable completion error was ignored")
	}
	if len(client.execCompletions) != 1 {
		t.Fatalf("completion attempts = %d, want 1", len(client.execCompletions))
	}
}

func TestComputerMaterializerCompletionRetriesServerError(t *testing.T) {
	client := &computerMaterializerTestClient{
		completeErrors: []error{
			computerMaterializerHTTPError(http.StatusServiceUnavailable),
		},
	}
	err := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CompleteErrorBackoff: time.Nanosecond,
	}).completeComputerBasicExec(
		context.Background(),
		client,
		workerapi.ComputerCommandCompleteRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.execCompletions) != 2 {
		t.Fatalf("completion attempts = %d, want 2", len(client.execCompletions))
	}
}

func TestComputerMaterializerFailsStartupWhenGuestDoesNotRegister(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestdChannelToken = "channel-token"
	computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		_, _, err := wire.ReadStreamFrameHeader(preparedServer)
		if err != nil {
			return
		}
		var request computerv0.MaterializeComputerRequest
		if err := frameio.ReadProtoFrame(preparedServer, &request); err != nil {
			return
		}
		artifactHeader, artifactSize, err := wire.ReadStreamFrameHeader(preparedServer)
		if err != nil || artifactHeader.Type != wire.StreamTypeComputerArtifact {
			return
		}
		_, _ = io.Copy(io.Discard, &io.LimitedReader{R: preparedServer, N: int64(artifactSize)})
		var buf [1]byte
		_, _ = preparedServer.Read(buf[:])
	}()
	client := &computerMaterializerTestClient{}
	session := &computerMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	pool := computerPreparedRuntimePool(t, computerMount, session)
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:            store,
		TempDir:        t.TempDir(),
		Heartbeat:      time.Hour,
		StartupTimeout: time.Millisecond,
		RuntimePool:    pool,
	}
	err := materializer.RunComputerMount(ctx, computerMount, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("materializer err = %v, want deadline exceeded", err)
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "computer_mount_startup_timeout") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestComputerMaterializerFailsComputerMountOnFatalHeartbeatError(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestdChannelToken = "channel-token"
	computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedComputerMount(t, preparedServer, computerMount, computerMount.ComputerInstanceID)
	client := &computerMaterializerTestClient{
		renewErrors: []error{errors.New("renew failed")},
	}
	session := &computerMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	pool := computerPreparedRuntimePool(t, computerMount, session)
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:         store,
		TempDir:     t.TempDir(),
		Heartbeat:   10 * time.Millisecond,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}
	err := materializer.RunComputerMount(ctx, computerMount, client)
	if err == nil || !strings.Contains(err.Error(), "renew computer mount") {
		t.Fatalf("materializer err = %v, want renew error", err)
	}
	if len(client.renews) == 0 || client.renews[0].EnvironmentID != computerMount.EnvironmentID || client.renews[0].ComputerInstanceID != computerMount.ComputerInstanceID || client.renews[0].WriterGeneration != computerMount.WriterGeneration {
		t.Fatalf("renew requests = %+v", client.renews)
	}
	if len(client.failures) != 1 || client.failures[0].ID != computerMount.ComputerInstanceID {
		t.Fatalf("failures = %+v", client.failures)
	}
}

func TestRunComputerMountCloseFailureReturnsOwnershipForPhysicalCleanup(t *testing.T) {
	for _, preservationFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("preservation_failure=%t", preservationFailure), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			preparedClient, preparedServer := net.Pipe()
			defer preparedServer.Close()
			store, computerMount := testComputerMountArtifacts(t)

			computerMount.OrgID = "org-1"
			computerMount.ComputerID = uuid.NewV7().String()
			computerMount.GuestdChannelToken = "channel-token"
			computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
			target := runtimeReservationTarget(computerMount.ComputerInstanceID, computerMount.RuntimeEpoch)
			target.Source.ComputerID = computerMount.ComputerID
			target.Source.WriterGeneration = computerMount.WriterGeneration
			target.Source.Computer = &workerapi.RuntimeComputerSource{VersionID: computerMount.Target.BaseComputerDiskVersionID}
			var closeFailure error = errors.New("prepared runtime cleanup failed")
			if preservationFailure {
				closeFailure = nil
			}
			session := &computerMaterializerTestSession{
				streams:   []io.ReadWriteCloser{preparedClient},
				operation: discardReadWriteCloser{},
				closeErr:  closeFailure,
			}
			pool := NewPreparedRuntimePool(nil, nil, 1, nil)
			pool.Reservations = newPreparedRuntimeReservations(t, 1)
			if err := pool.reserveRuntimeCapacity(target); err != nil {
				t.Fatal(err)
			}
			key := computerInstanceIDFromComputerMount(computerMount)
			ready := newPreparedRuntimeSignal()
			ready.finish(nil)
			pool.entries[key] = []preparedRuntimeEntry{{
				session: session, poolKey: key, computerInstanceID: target.ID,
				runtimeEpoch: target.WorkerEpoch, target: target,
				exit: newPreparedRuntimeSignal(), ready: ready,
			}}
			go acknowledgePreparedComputerMount(t, preparedServer, computerMount, key)
			sessions := NewComputerMountSessions()
			client := &computerMaterializerTestClient{onReady: func() {
				if preservationFailure {
					_, pending := newSaveHostFixture(t, "capture")
					closeFailure = pending.Wait(context.Background())
					sessions.mu.RLock()
					managed := sessions.sessions[computerMount.ComputerInstanceID].session.(*managedComputerMountSession)
					sessions.mu.RUnlock()
					managed.saves.mu.Lock()
					managed.saves.pending = pending
					managed.saves.mu.Unlock()
				}
				cancel()
			}}
			materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
				Sessions:    sessions,
				CAS:         store,
				TempDir:     t.TempDir(),
				Heartbeat:   time.Hour,
				PollEvery:   time.Hour,
				RuntimePool: pool,
			}

			err := materializer.RunComputerMount(ctx, computerMount, client)
			if len(client.execClaims) != 1 {
				t.Fatalf("mounted requests = %d, want 1", len(client.execClaims))
			}
			if !errors.Is(err, closeFailure) {
				t.Fatalf("materializer error = %v, want close failure", err)
			}
			if pool.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
				t.Fatal("exited materializer retained checkout ownership")
			}
			if got := len(pool.Reservations.Snapshot().Reservations); got != 1 {
				t.Fatalf("capacity reservations after close failure = %d, want 1", got)
			}
			connector := &cleanupRuntimeBackend{err: errors.New("process still alive")}
			pool.Backend = connector
			control := &typedRuntimeClient{}
			if err := pool.StopRuntimeTarget(context.Background(), control, target); err == nil {
				t.Fatal("unproved host cleanup succeeded")
			}
			if len(pool.Reservations.Snapshot().Reservations) != 1 || len(control.closed) != 0 {
				t.Fatal("released capacity or published proof before physical cleanup")
			}
			connector.err = nil
			if err := pool.StopRuntimeTarget(context.Background(), control, target); err != nil {
				t.Fatal(err)
			}
			if len(pool.Reservations.Snapshot().Reservations) != 0 || len(control.closed) != 1 || control.closed[0].CleanupProof == nil {
				t.Fatal("physical cleanup did not release capacity and publish proof")
			}

		})
	}
}

func TestComputerMaterializerFailsComputerMountWhenSessionExits(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	exit := make(chan error, 1)
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestdChannelToken = "channel-token"
	computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go func() {
		acknowledgePreparedComputerMount(t, preparedServer, computerMount, computerMount.ComputerInstanceID)
		exit <- errors.New("the Firecracker exited")
	}()
	client := &computerMaterializerTestClient{}
	session := &computerMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
		exit:      exit,
	}
	pool := computerPreparedRuntimePool(t, computerMount, session)
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:         store,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Hour,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}
	err := materializer.RunComputerMount(ctx, computerMount, client)
	if err == nil || !strings.Contains(err.Error(), "computer mount VM exited") {
		t.Fatalf("materializer err = %v, want VM exit", err)
	}
	if len(client.failures) != 1 || client.failures[0].ID != computerMount.ComputerInstanceID {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "computer_mount_vm_exited") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestComputerMaterializerOwnsProgramStartFailureCleanup(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestdChannelToken = "channel-token"
	computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedComputerMount(
		t,
		preparedServer,
		computerMount,
		computerMount.ComputerInstanceID,
	)
	rawSession := &computerMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	pool := computerPreparedRuntimePool(t, computerMount, rawSession)
	sessions := NewComputerMountSessions()
	mounted := make(chan struct{})
	client := &computerMaterializerTestClient{onReady: func() { close(mounted) }}
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:         store,
		Sessions:    sessions,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Hour,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}
	result := make(chan error, 1)
	go func() {
		result <- materializer.RunComputerMount(ctx, computerMount, client)
	}()
	select {
	case <-mounted:
	case <-time.After(5 * time.Second):
		t.Fatal("Computer Mount did not become ready")
	}
	if err := sessions.FailComputerInstanceSession(ctx, computerMount.ComputerInstanceID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "failed before start proof") {
			t.Fatalf("materializer error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Computer Mount owner did not finish Program start failure")
	}
	if rawSession.closeCount() == 0 {
		t.Fatal("Computer Mount VM was not closed")
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "computer_mount_program_start_failed") ||
		strings.Contains(got, "exec image runtime") {
		t.Fatalf("failure error = %s", got)
	}
	if got := len(pool.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("capacity reservations = %d, want 0", got)
	}
}

func TestComputerMaterializerProgramStartFailureKeepsCapacityWhenRuntimeCloseFails(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestdChannelToken = "channel-token"
	computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedComputerMount(
		t,
		preparedServer,
		computerMount,
		computerMount.ComputerInstanceID,
	)
	rawCause := "signed-url-secret-sentinel"
	rawSession := &computerMaterializerTestSession{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
		closeErr:  errors.New(rawCause),
	}
	pool := computerPreparedRuntimePool(t, computerMount, rawSession)
	sessions := NewComputerMountSessions()
	mounted := make(chan struct{})
	client := &computerMaterializerTestClient{onReady: func() { close(mounted) }}
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:         store,
		Sessions:    sessions,
		TempDir:     t.TempDir(),
		Heartbeat:   time.Hour,
		PollEvery:   time.Hour,
		RuntimePool: pool,
	}
	result := make(chan error, 1)
	go func() {
		result <- materializer.RunComputerMount(ctx, computerMount, client)
	}()
	select {
	case <-mounted:
	case <-time.After(5 * time.Second):
		t.Fatal("Computer Mount did not become ready")
	}
	if err := sessions.FailComputerInstanceSession(ctx, computerMount.ComputerInstanceID); err == nil ||
		!strings.Contains(err.Error(), rawCause) {
		t.Fatalf("failure request error = %v, want local cleanup cause", err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "computer mount runtime cleanup failed") {
			t.Fatalf("materializer error = %v, want static cleanup failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Computer Mount owner did not finish Program start failure")
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "computer_mount_runtime_close_failed") ||
		strings.Contains(got, rawCause) {
		t.Fatalf("failure error = %s", got)
	}
	if got := len(pool.Reservations.Snapshot().Reservations); got != 1 {
		t.Fatalf("capacity reservations = %d, want 1 until cleanup is proven", got)
	}
}

func TestComputerMaterializerRegistersPreparedRuntimeOverOpenedStream(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	_, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestdChannelToken = "channel-token"
	computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	session := &computerMaterializerTestSession{
		streams: []io.ReadWriteCloser{preparedClient},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		acknowledgePreparedComputerMount(t, preparedServer, computerMount, "runtime-key")
	}()

	err := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).registerComputerMount(ctx, session, computerMount, "runtime-key")
	if err != nil {
		t.Fatal(err)
	}
	<-done
	opened := session.openedStreams()
	if len(opened) != 1 || opened[0] != preparedClient {
		t.Fatalf("opened streams = %+v, want prepared runtime computerMount over OpenStream", opened)
	}
}

func TestComputerMaterializerValidatesSuccessReceiptsOnlyAfterRunningState(t *testing.T) {
	_, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestdChannelToken = "channel-token"
	computerMount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))

	tests := []struct {
		name     string
		response func(*computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse
		want     string
		notWant  string
	}{
		{
			name: "failed with phase error",
			response: func(*computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
				return &computerv0.MaterializeComputerResponse{
					Status: "failed",
					Phases: []*computerv0.ComputerMountPhase{{
						Name:  "guest_computer_target_verify",
						Error: "computer tree digest mismatch",
					}},
				}
			},
			want:    "guest_computer_target_verify: computer tree digest mismatch",
			notWant: "target does not match",
		},
		{
			name: "failed without phase error",
			response: func(*computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
				return &computerv0.MaterializeComputerResponse{Status: "failed"}
			},
			want: `computer materialize returned state "failed"`,
		},
		{
			name: "running without target",
			response: func(*computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
				return &computerv0.MaterializeComputerResponse{Status: "running", GuestdChannelTokenHash: computerMount.GuestdChannelTokenHash}
			},
			want: "target does not match",
		},
		{
			name: "running without channel receipt",
			response: func(request *computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
				return &computerv0.MaterializeComputerResponse{Status: "running", Target: request.Target}
			},
			want: "guest channel token hash mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preparedClient, preparedServer := net.Pipe()
			defer preparedServer.Close()
			go respondToPreparedComputerMountWithRequest(t, preparedServer, test.response)
			err := (ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).registerComputerMount(context.Background(), &computerMaterializerTestSession{
				streams: []io.ReadWriteCloser{preparedClient},
			}, computerMount, "runtime-key")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("register error = %v, want %q", err, test.want)
			}
			if test.notWant != "" && strings.Contains(err.Error(), test.notWant) {
				t.Fatalf("register error = %v, do not want %q", err, test.notWant)
			}
		})
	}
}

func respondToPreparedComputerMountWithRequest(t *testing.T, stream io.ReadWriteCloser, response func(*computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse) {
	t.Helper()
	header, _, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read materialize header: %v", err)
		return
	}
	if header.Type != wire.StreamTypeComputerMaterialize {
		t.Errorf("materialize header = %+v", header)
		return
	}
	var request computerv0.MaterializeComputerRequest
	if err := frameio.ReadProtoFrame(stream, &request); err != nil {
		t.Errorf("read materialize request: %v", err)
		return
	}
	if err := frameio.WriteProtoFrame(stream, response(&request)); err != nil {
		t.Errorf("write materialize response: %v", err)
	}
}

func acknowledgePreparedComputerMount(t *testing.T, stream io.ReadWriteCloser, computerMount workerapi.ComputerInstanceAssignment, runtimeKey string) {
	t.Helper()
	respondToPreparedComputerMountWithRequest(t, stream, func(request *computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
		if !request.UsePreparedRuntime || request.GetEnvelope().GetComputerInstanceId() != runtimeKey {
			t.Errorf("prepared runtime request use=%v computer_instance_id=%q", request.UsePreparedRuntime, request.GetEnvelope().GetComputerInstanceId())
		}
		return &computerv0.MaterializeComputerResponse{
			Status:                 "running",
			GuestdChannelTokenHash: computerMount.GuestdChannelTokenHash,
			Target:                 request.Target,
		}
	})
}

type computerMaterializerTestSession struct {
	mu        sync.Mutex
	operation io.ReadWriteCloser
	streams   []io.ReadWriteCloser
	opened    []io.ReadWriteCloser
	exit      <-chan error
	closeErr  error
	closed    int
}

func (s *computerMaterializerTestSession) Stream() vm.Stream {
	return nil
}

func (s *computerMaterializerTestSession) OpenStream(context.Context) (vm.Stream, error) {
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

func (s *computerMaterializerTestSession) Close(context.Context) error {
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

func (s *computerMaterializerTestSession) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *computerMaterializerTestSession) openedStreams() []io.ReadWriteCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]io.ReadWriteCloser(nil), s.opened...)
}

func (s *computerMaterializerTestSession) Wait(ctx context.Context) error {
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

type computerMaterializerTestClient struct {
	commandMu       sync.Mutex
	commandLogs     []workerapi.CommandLogAppendRequest
	appendErrors    []error
	failErrors      []error
	cancel          context.CancelFunc
	computerCommand *workerapi.ComputerCommand
	execClaims      []workerapi.ComputerCommandClaimRequest
	execCompletions []workerapi.ComputerCommandCompleteRequest
	completeErrors  []error
	renewErrors     []error
	renews          []workerapi.ComputerInstanceRenewRequest
	renewed         chan struct{}
	renewedOnce     sync.Once
	onReady         func()
	readyOnce       sync.Once
	stops           int
	closed          []workerapi.ComputerInstanceStateRequest
	failures        []workerapi.ComputerInstanceStateRequest
}

func (c *computerMaterializerTestClient) RenewComputerInstance(_ context.Context, request workerapi.ComputerInstanceRenewRequest) (workerapi.ComputerInstanceRenewResponse, error) {
	c.renews = append(c.renews, request)
	if c.renewed != nil {
		c.renewedOnce.Do(func() { close(c.renewed) })
	}
	if len(c.renewErrors) > 0 {
		err := c.renewErrors[0]
		c.renewErrors = c.renewErrors[1:]
		return workerapi.ComputerInstanceRenewResponse{}, err
	}
	return workerapi.ComputerInstanceRenewResponse{ComputerInstanceID: request.ComputerInstanceID, WriterGeneration: request.WriterGeneration, DesiredState: "ready", DesiredVersion: 1, WriterExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (c *computerMaterializerTestClient) MarkComputerInstanceClosed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.stops++
	c.closed = append(c.closed, request)
	return workerapi.ComputerInstance{}, nil
}

func (c *computerMaterializerTestClient) MarkComputerInstanceFailed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.failures = append(c.failures, request)
	if len(c.failErrors) > 0 {
		err := c.failErrors[0]
		c.failErrors = c.failErrors[1:]
		return workerapi.ComputerInstance{}, err
	}
	return workerapi.ComputerInstance{}, nil
}

func (c *computerMaterializerTestClient) ClaimComputerCommand(_ context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	c.execClaims = append(c.execClaims, request)
	c.readyOnce.Do(func() {
		if c.onReady != nil {
			c.onReady()
		}
	})
	return workerapi.ComputerCommandClaimResponse{Command: c.computerCommand}, nil
}

func (c *computerMaterializerTestClient) CompleteComputerCommand(_ context.Context, request workerapi.ComputerCommandCompleteRequest) error {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	c.execCompletions = append(c.execCompletions, request)
	if len(c.completeErrors) > 0 {
		err := c.completeErrors[0]
		c.completeErrors = c.completeErrors[1:]
		return err
	}
	c.computerCommand = nil
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

type computerMaterializerHTTPError int

func (e computerMaterializerHTTPError) Error() string {
	return http.StatusText(int(e))
}

func (e computerMaterializerHTTPError) HTTPStatusCode() int {
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
	store, mount := testComputerMountArtifacts(t)
	cache, temp := t.TempDir(), t.TempDir()
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, ArtifactCacheDir: cache}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Go(func() {
			for j := 0; j < 10; j++ {
				path, cleanup, err := materializer.restoreCASObject(t.Context(), temp, "image", mount.ComputerImage)
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
	store, mount := testComputerMountArtifacts(t)
	mount.OrgID, mount.ComputerID = "org", uuid.NewV7().String()
	mount.GuestdChannelToken = "channel-token"
	mount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte("channel-token"))
	go acknowledgePreparedComputerMount(t, server, mount, mount.ComputerInstanceID)
	stopErr, reportErr := errors.New("VM stop unproved"), errors.New("failure acknowledgement lost")
	raw := &computerMaterializerTestSession{streams: []io.ReadWriteCloser{conn}, operation: discardReadWriteCloser{}, closeErr: stopErr}
	pool := computerPreparedRuntimePool(t, mount, raw)
	sessions := NewComputerMountSessions()
	mounted := make(chan struct{})
	client := &computerMaterializerTestClient{onReady: func() { close(mounted) }, failErrors: []error{reportErr}}
	materializer := ComputerMaterializer{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, Sessions: sessions, TempDir: t.TempDir(), Heartbeat: time.Hour, PollEvery: time.Hour, RuntimePool: pool}
	done := make(chan error, 1)
	go func() { done <- materializer.RunComputerMount(ctx, mount, client) }()
	select {
	case <-mounted:
	case <-ctx.Done():
		t.Fatal("mount not ready")
	}
	borrowed, err := sessions.OpenComputerInstanceSession(ctx, mount.ComputerInstanceID)
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
	if pool.runtimeCheckedOut(mount.ComputerInstanceID, mount.RuntimeEpoch) {
		t.Fatal("exited materializer retained checkout ownership")
	}
	if len(pool.Reservations.Snapshot().Reservations) != 1 {
		t.Fatal("released capacity before physical reclaim")
	}
	if raw.closeCount() != 1 {
		t.Fatal("retried cached session close instead of deferring physical reclaim")
	}
	// Reconciliation receives a CP-authorized target after active leases expire.
	// It must use host cleanup, not retry the cached session Close failure.
	connector := &cleanupRuntimeBackend{err: errors.New("process still alive")}
	pool.Backend = connector
	target := runtimeReservationTarget(mount.ComputerInstanceID, mount.RuntimeEpoch)
	target.Source.ComputerID = mount.ComputerID
	target.Source.Computer = &workerapi.RuntimeComputerSource{VersionID: mount.Target.BaseComputerDiskVersionID}
	control := &typedRuntimeClient{}
	if err := pool.ReclaimFailedRuntimeTarget(ctx, control, target); err == nil {
		t.Fatal("unproved host cleanup succeeded")
	}
	if len(pool.Reservations.Snapshot().Reservations) != 1 || len(control.failed) != 0 {
		t.Fatal("released or published proof before physical cleanup")
	}
	connector.err = nil
	if err := pool.ReclaimFailedRuntimeTarget(ctx, control, target); err != nil {
		t.Fatal(err)
	}
	if pool.runtimeCheckedOut(mount.ComputerInstanceID, mount.RuntimeEpoch) || len(pool.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("retained runtime after proved cleanup")
	}
	if len(control.failed) != 1 || control.failed[0].CleanupProof == nil || raw.closeCount() != 1 {
		t.Fatal("reclaim did not publish host proof independently of cached Close")
	}
}

func TestPreparedComputerCheckoutRejectsChangedIdentityWithoutConsuming(t *testing.T) {
	_, mount := testComputerMountArtifacts(t)
	mount.ComputerID = "computer-1"
	session := &computerMaterializerTestSession{}
	pool := computerPreparedRuntimePool(t, mount, session)
	for _, change := range []func(*workerapi.ComputerInstanceAssignment){func(m *workerapi.ComputerInstanceAssignment) { m.ComputerID = "computer-2" }, func(m *workerapi.ComputerInstanceAssignment) { m.Target.BaseComputerDiskVersionID = "other-version" }} {
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

func (c *computerMaterializerTestClient) AppendCommandLog(_ context.Context, request workerapi.CommandLogAppendRequest) error {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	c.commandLogs = append(c.commandLogs, request)
	if len(c.appendErrors) > 0 {
		err := c.appendErrors[0]
		c.appendErrors = c.appendErrors[1:]
		return err
	}
	return nil
}

type continuingCommandClient struct {
	computerMaterializerTestClient
	afterCompletion func()
}

func (c *continuingCommandClient) ClaimComputerCommand(ctx context.Context, r workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	c.commandMu.Lock()
	completed := len(c.execCompletions) > 0
	c.commandMu.Unlock()
	if completed && c.afterCompletion != nil {
		c.afterCompletion()
		c.afterCompletion = nil
	}
	return c.computerMaterializerTestClient.ClaimComputerCommand(ctx, r)
}

func TestCommandCompletionLeavesComputerServing(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	physical := &computerMaterializerTestSession{operation: host}
	managed := newManagedComputerMountSession(physical)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	mount := workerapi.ComputerInstanceAssignment{OrgID: "org-1", ComputerID: "computer-1", ComputerInstanceID: "instance-1", WriterGeneration: 2, GuestdChannelToken: "token"}
	command := workerapi.ComputerCommand{CommandID: "command-1", ComputerID: mount.ComputerID, ComputerInstanceID: mount.ComputerInstanceID, RequestFingerprint: strings.Repeat("a", 64), Request: json.RawMessage(`{"command":["true"]}`), WriterGeneration: 2, ExpiresAt: time.Now().Add(time.Minute)}
	guestDone := make(chan error, 1)
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(guest); err != nil {
			guestDone <- err
			return
		}
		var request computerv0.ComputerBasicExecRequest
		if err := frameio.ReadProtoFrame(guest, &request); err != nil {
			guestDone <- err
			return
		}
		guestDone <- frameio.WriteProtoFrame(guest, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: command.RequestFingerprint}}})
	}()
	continued := false
	client := &continuingCommandClient{computerMaterializerTestClient: computerMaterializerTestClient{computerCommand: &command}}
	client.afterCompletion = func() { continued = true; cancel() }
	m := ComputerMaterializer{PollEvery: time.Millisecond}
	renewal := m.startRenewalLoop(ctx, workerapi.ComputerInstanceRenewRequest{}, client, time.Hour)
	err := m.serveComputerMount(ctx, renewal, managed, mount, client, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("serve exit=%v", err)
	}
	if err = <-guestDone; err != nil {
		t.Fatal(err)
	}
	if !continued || len(client.execCompletions) != 1 || client.stops != 0 || physical.closeCount() != 0 {
		t.Fatalf("continued=%v completions=%d stops=%d closes=%d", continued, len(client.execCompletions), client.stops, physical.closeCount())
	}
	managed.saves.mu.Lock()
	stopped := managed.saves.stopped
	managed.saves.mu.Unlock()
	if stopped {
		t.Fatal("Command completion stopped Computer preservation")
	}
}

func (c *computerMaterializerTestClient) ReconcileComputerCommand(context.Context, workerapi.ComputerCommandCompleteRequest) error {
	return nil
}

func (*computerMaterializerTestClient) GetComputerRunCleanup(context.Context, workerapi.ComputerRunCleanupRequest) (workerapi.ComputerRunCleanupResponse, error) {
	return workerapi.ComputerRunCleanupResponse{}, nil
}
func (*computerMaterializerTestClient) ReconcileComputerRun(context.Context, workerapi.ComputerRunReconcileRequest) error {
	return nil
}
