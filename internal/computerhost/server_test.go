package computerhost

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

func TestServerUsesStartupTimeout(t *testing.T) {
	if got := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).startupTimeout(); got != computerStartupTimeout {
		t.Fatalf("startup timeout = %s, want %s", got, computerStartupTimeout)
	}
	custom := time.Second
	if got := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, StartupTimeout: custom}).startupTimeout(); got != custom {
		t.Fatalf("custom startup timeout = %s, want %s", got, custom)
	}
}

func TestServerRenewsWhileAwaitingPreparedMachine(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			store, mount := testComputerMountArtifacts(t)
			mount.ComputerInstanceID, mount.OrgID = "mount-await-ready", "org-1"
			machines := computerPreparedMachines(t, mount, &serverTestMachine{})
			key := computerInstanceIDFromComputerMount(mount)
			machines.entries[key][0].ready = newPreparedMachineSignal()
			renewed := make(chan struct{})
			client := &serverTestClient{renewed: renewed}
			if fail {
				client.renewErrors = []error{errors.New("renew failed")}
			}
			server := Server{RestoreControl: unusedComputerRestoreControl{}, Mounts: NewMounts(), ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, Heartbeat: time.Millisecond, Machines: machines}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx, mount, client) }()
			if !fail {
				select {
				case <-renewed:
				case <-time.After(time.Second):
					t.Fatal("pending instance admission did not renew")
				}
				cancel()
			}
			select {
			case err := <-done:
				if fail && (err == nil || !strings.Contains(err.Error(), "renew computer mount")) {
					t.Fatalf("renew failure=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("pending instance admission did not cancel")
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
		WorkerEpoch:        1,
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

func computerPreparedMachines(t *testing.T, mount workerapi.ComputerInstanceAssignment, machine liveCaptureMachine) *PreparedMachines {
	t.Helper()
	target := instanceReservationTarget(mount.ComputerInstanceID, mount.WorkerEpoch)
	target.Source.ComputerID = mount.ComputerID
	target.Source.WriterGeneration = mount.WriterGeneration
	target.Source.Computer = &workerapi.InstanceComputerSource{VersionID: mount.Target.BaseComputerDiskVersionID}
	machines := NewPreparedMachines(nil, nil, 1, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	if err := machines.reserveInstanceCapacity(target); err != nil {
		t.Fatal(err)
	}
	key := computerInstanceIDFromComputerMount(mount)
	ready := newPreparedMachineSignal()
	ready.finish(nil)
	machines.entries[key] = []preparedMachineEntry{{
		machine: machine, machineKey: key, computerInstanceID: target.ID,
		workerEpoch: target.WorkerEpoch, target: target,
		exit: newPreparedMachineSignal(), ready: ready,
	}}
	return machines
}

func TestServerRestoreCASObjectUsesLocalCache(t *testing.T) {
	store, computerMount := testComputerMountArtifacts(t)
	cacheDir := t.TempDir()
	tempDir := t.TempDir()
	server := Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:              store,
		ArtifactCacheDir: cacheDir,
	}

	_, firstCleanup, err := server.restoreCASObject(context.Background(), tempDir, "computer-image", computerMount.ComputerImage)
	if err != nil {
		t.Fatal(err)
	}
	firstCleanup()
	secondPath, secondCleanup, err := server.restoreCASObject(context.Background(), tempDir, "computer-image", computerMount.ComputerImage)
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

func TestServerRestoreCASObjectRefreshesInvalidLocalCache(t *testing.T) {
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
	server := Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:              store,
		ArtifactCacheDir: cacheDir,
	}

	path, cleanup, err := server.restoreCASObject(context.Background(), tempDir, "computer-image", computerMount.ComputerImage)
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

func TestServerChecksOutPreparedMachine(t *testing.T) {
	store, computerMount := testComputerMountArtifacts(t)
	wantMachine := &serverTestMachine{}
	machines := computerPreparedMachines(t, computerMount, wantMachine)
	server := Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:      store,
		TempDir:  t.TempDir(),
		Machines: machines,
	}

	checkout, computerInstanceID, err := server.materializeMachine(context.Background(), &computerMount)
	if err != nil {
		t.Fatal(err)
	}
	if machine := checkout.Machine(); machine != wantMachine {
		t.Fatalf("machine = %T %p, want %T %p", machine, machine, wantMachine, wantMachine)
	}
	if computerInstanceID != computerMount.ComputerInstanceID {
		t.Fatalf("instance id = %q, want %q", computerInstanceID, computerMount.ComputerInstanceID)
	}
	if !machines.instanceCheckedOut(computerMount.ComputerInstanceID, computerMount.WorkerEpoch) {
		t.Fatal("prepared machine was not checked out")
	}
	if got := store.getCalls[computerMount.ComputerImage.Digest]; got != 0 {
		t.Fatalf("computer image CAS gets = %d, want 0", got)
	}
	if len(store.getCalls) != 0 {
		t.Fatalf("prepared computer unexpectedly read CAS: %+v", store.getCalls)
	}
	if err := checkout.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestServerReleasesCheckoutOnRestoreProvenanceFailure(t *testing.T) {
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
			machine := &serverTestMachine{}
			machines := computerPreparedMachines(t, mount, machine)
			key := computerInstanceIDFromComputerMount(mount)
			machines.entries[key][0].target.Source.Restore = &workerapi.InstanceRestore{
				CheckpointID: "checkpoint-b",
			}
			server := Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, Machines: machines}

			_, _, err := server.materializeMachine(context.Background(), &mount)
			var failure computerMountFailure
			if !errors.As(err, &failure) || failure.code != test.wantCode {
				t.Fatalf("materialize error = %v, want %s", err, test.wantCode)
			}
			if machine.closeCount() != 1 {
				t.Fatalf("machine close count = %d, want 1", machine.closeCount())
			}
			if machines.instanceCheckedOut(mount.ComputerInstanceID, mount.WorkerEpoch) {
				t.Fatal("failed restore provenance retained instance checkout")
			}
			if got := len(machines.Reservations.Snapshot().Reservations); got != 0 {
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

func TestServerFailsWhenPreparedMachineIsMissing(t *testing.T) {
	store, computerMount := testComputerMountArtifacts(t)
	server := Server{RestoreControl: unusedComputerRestoreControl{}, Mounts: NewMounts(), ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:      store,
		Machines: NewPreparedMachines(nil, nil, 1, nil),
	}
	client := &serverTestClient{}

	err := server.Serve(context.Background(), computerMount, client)
	if err == nil {
		t.Fatal("missing prepared machine was accepted")
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

func TestServerPreparedComputerSkipsComputerCAS(t *testing.T) {
	store, mount := testComputerMountArtifacts(t)
	mount.Target = workerapi.ComputerMountTarget{
		BaseComputerDiskVersionID: mount.Target.BaseComputerDiskVersionID,
	}
	machine := &serverTestMachine{}
	machines := computerPreparedMachines(t, mount, machine)
	server := Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:      store,
		Machines: machines,
	}

	checkout, _, err := server.materializeMachine(context.Background(), &mount)
	if err != nil {
		t.Fatal(err)
	}
	if gotMachine := checkout.Machine(); gotMachine != machine {
		t.Fatalf("machine = %v, want prepared machine", gotMachine)
	}
	if got := store.getCalls[mount.ComputerImage.Digest]; got != 0 {
		t.Fatalf("computer image CAS gets = %d, want 0", got)
	}
	if err := checkout.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestServerDispatchesBasicExec(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	machine := &serverTestMachine{operation: clientStream}
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
			request.GetEnvelope().GetChannelCredential() != "channel-credential" ||
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
	client := &serverTestClient{}
	completion, err := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).dispatchComputerBasicExec(
		context.Background(),
		machine,
		workerapi.ComputerInstanceAssignment{
			OrgID: "org-1", ComputerID: "computer-1", ComputerInstanceID: "instance-1", WriterGeneration: 4,
			GuestChannelCredential: "channel-credential",
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

func TestServerRejectsMismatchedBasicExecClaim(t *testing.T) {
	_, err := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).dispatchComputerBasicExec(
		context.Background(),
		&serverTestMachine{},
		workerapi.ComputerInstanceAssignment{
			ComputerID:             "computer-1",
			GuestChannelCredential: "channel-credential",
		},
		workerapi.ComputerCommand{
			CommandID: "process-1", ComputerInstanceID: "instance-2",
			ComputerID: "computer-1", RequestFingerprint: strings.Repeat("a", 64),
			WriterGeneration: 1,
			ExpiresAt:        time.Now().Add(time.Minute),
		}, &serverTestClient{},
	)
	var protocolError *computerBasicExecProtocolError
	if !errors.As(err, &protocolError) {
		t.Fatalf("error = %v, want protocol error", err)
	}
}

func TestServerRejectsGuestAuthorityOutcomes(t *testing.T) {
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

func TestServerCompletionStopsOnNonRetryableError(t *testing.T) {
	client := &serverTestClient{
		completeErrors: []error{serverHTTPError(http.StatusBadRequest)},
	}
	err := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
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

func TestServerCompletionRetriesServerError(t *testing.T) {
	client := &serverTestClient{
		completeErrors: []error{
			serverHTTPError(http.StatusServiceUnavailable),
		},
	}
	err := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
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

func TestServerFailsStartupWhenGuestDoesNotRegister(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestChannelCredential = "channel-credential"
	computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
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
	client := &serverTestClient{}
	machine := &serverTestMachine{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	machines := computerPreparedMachines(t, computerMount, machine)
	server := Server{RestoreControl: unusedComputerRestoreControl{}, Mounts: NewMounts(), ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:            store,
		TempDir:        t.TempDir(),
		Heartbeat:      time.Hour,
		StartupTimeout: time.Millisecond,
		Machines:       machines,
	}
	err := server.Serve(ctx, computerMount, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("server err = %v, want deadline exceeded", err)
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "computer_mount_startup_timeout") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestServerFailsComputerMountOnFatalHeartbeatError(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestChannelCredential = "channel-credential"
	computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
	go acknowledgePreparedComputerMount(t, preparedServer, computerMount, computerMount.ComputerInstanceID)
	client := &serverTestClient{
		renewErrors: []error{errors.New("renew failed")},
	}
	machine := &serverTestMachine{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	machines := computerPreparedMachines(t, computerMount, machine)
	server := Server{RestoreControl: unusedComputerRestoreControl{}, Mounts: NewMounts(), ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:       store,
		TempDir:   t.TempDir(),
		Heartbeat: 10 * time.Millisecond,
		PollEvery: time.Hour,
		Machines:  machines,
	}
	err := server.Serve(ctx, computerMount, client)
	if err == nil || !strings.Contains(err.Error(), "renew computer mount") {
		t.Fatalf("server err = %v, want renew error", err)
	}
	if len(client.renews) == 0 || client.renews[0].EnvironmentID != computerMount.EnvironmentID || client.renews[0].ComputerInstanceID != computerMount.ComputerInstanceID || client.renews[0].WriterGeneration != computerMount.WriterGeneration {
		t.Fatalf("renew requests = %+v", client.renews)
	}
	if len(client.failures) != 1 || client.failures[0].ID != computerMount.ComputerInstanceID {
		t.Fatalf("failures = %+v", client.failures)
	}
}

func TestServeCloseFailureReturnsOwnershipForPhysicalCleanup(t *testing.T) {
	for _, preservationFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("preservation_failure=%t", preservationFailure), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			preparedClient, preparedServer := net.Pipe()
			defer preparedServer.Close()
			store, computerMount := testComputerMountArtifacts(t)

			computerMount.OrgID = "org-1"
			computerMount.ComputerID = uuid.NewV7().String()
			computerMount.GuestChannelCredential = "channel-credential"
			computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
			target := instanceReservationTarget(computerMount.ComputerInstanceID, computerMount.WorkerEpoch)
			target.Source.ComputerID = computerMount.ComputerID
			target.Source.WriterGeneration = computerMount.WriterGeneration
			target.Source.Computer = &workerapi.InstanceComputerSource{VersionID: computerMount.Target.BaseComputerDiskVersionID}
			var closeFailure error = errors.New("prepared machine cleanup failed")
			if preservationFailure {
				closeFailure = nil
			}
			machine := &serverTestMachine{
				streams:   []io.ReadWriteCloser{preparedClient},
				operation: discardReadWriteCloser{},
				closeErr:  closeFailure,
			}
			machines := NewPreparedMachines(nil, nil, 1, nil)
			machines.Reservations = newPreparedMachineReservations(t, 1)
			if err := machines.reserveInstanceCapacity(target); err != nil {
				t.Fatal(err)
			}
			key := computerInstanceIDFromComputerMount(computerMount)
			ready := newPreparedMachineSignal()
			ready.finish(nil)
			machines.entries[key] = []preparedMachineEntry{{
				machine: machine, machineKey: key, computerInstanceID: target.ID,
				workerEpoch: target.WorkerEpoch, target: target,
				exit: newPreparedMachineSignal(), ready: ready,
			}}
			go acknowledgePreparedComputerMount(t, preparedServer, computerMount, key)
			mounts := NewMounts()
			client := &serverTestClient{onReady: func() {
				if preservationFailure {
					_, pending := newSaveHostFixture(t, "capture")
					closeFailure = pending.Wait(context.Background())
					mounts.mu.RLock()
					managed := mounts.mounts[computerMount.ComputerInstanceID].instance
					mounts.mu.RUnlock()
					managed.saves.mu.Lock()
					managed.saves.pending = pending
					managed.saves.mu.Unlock()
				}
				cancel()
			}}
			server := Server{RestoreControl: unusedComputerRestoreControl{}, ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
				Mounts:    mounts,
				CAS:       store,
				TempDir:   t.TempDir(),
				Heartbeat: time.Hour,
				PollEvery: time.Hour,
				Machines:  machines,
			}

			err := server.Serve(ctx, computerMount, client)
			if len(client.execClaims) != 1 {
				t.Fatalf("mounted requests = %d, want 1", len(client.execClaims))
			}
			if !errors.Is(err, closeFailure) {
				t.Fatalf("server error = %v, want close failure", err)
			}
			if machines.instanceCheckedOut(target.ID, target.WorkerEpoch) {
				t.Fatal("exited server retained checkout ownership")
			}
			if got := len(machines.Reservations.Snapshot().Reservations); got != 1 {
				t.Fatalf("capacity reservations after close failure = %d, want 1", got)
			}
			connector := &cleanupBackend{err: errors.New("process still alive")}
			machines.Backend = connector
			control := &typedInstanceClient{}
			if err := machines.stopInstanceTarget(context.Background(), control, target); err == nil {
				t.Fatal("unproved host cleanup succeeded")
			}
			if len(machines.Reservations.Snapshot().Reservations) != 1 || len(control.closed) != 0 {
				t.Fatal("released capacity or published proof before physical cleanup")
			}
			connector.err = nil
			if err := machines.stopInstanceTarget(context.Background(), control, target); err != nil {
				t.Fatal(err)
			}
			if len(machines.Reservations.Snapshot().Reservations) != 0 || len(control.closed) != 1 || control.closed[0].CleanupProof == nil {
				t.Fatal("physical cleanup did not release capacity and publish proof")
			}

		})
	}
}

func TestServerFailsComputerMountWhenMachineExits(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	exit := make(chan error, 1)
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestChannelCredential = "channel-credential"
	computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
	go func() {
		acknowledgePreparedComputerMount(t, preparedServer, computerMount, computerMount.ComputerInstanceID)
		exit <- errors.New("the Firecracker exited")
	}()
	client := &serverTestClient{}
	machine := &serverTestMachine{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
		exit:      exit,
	}
	machines := computerPreparedMachines(t, computerMount, machine)
	server := Server{RestoreControl: unusedComputerRestoreControl{}, Mounts: NewMounts(), ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:       store,
		TempDir:   t.TempDir(),
		Heartbeat: time.Hour,
		PollEvery: time.Hour,
		Machines:  machines,
	}
	err := server.Serve(ctx, computerMount, client)
	if err == nil || !strings.Contains(err.Error(), "computer mount VM exited") {
		t.Fatalf("server err = %v, want VM exit", err)
	}
	if len(client.failures) != 1 || client.failures[0].ID != computerMount.ComputerInstanceID {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "computer_mount_vm_exited") {
		t.Fatalf("failure error = %s", got)
	}
}

func TestServerOwnsProgramStartFailureCleanup(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestChannelCredential = "channel-credential"
	computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
	go acknowledgePreparedComputerMount(
		t,
		preparedServer,
		computerMount,
		computerMount.ComputerInstanceID,
	)
	rawMachine := &serverTestMachine{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
	}
	machines := computerPreparedMachines(t, computerMount, rawMachine)
	mounts := NewMounts()
	mounted := make(chan struct{})
	client := &serverTestClient{onReady: func() { close(mounted) }}
	server := Server{RestoreControl: unusedComputerRestoreControl{}, ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:       store,
		Mounts:    mounts,
		TempDir:   t.TempDir(),
		Heartbeat: time.Hour,
		PollEvery: time.Hour,
		Machines:  machines,
	}
	result := make(chan error, 1)
	go func() {
		result <- server.Serve(ctx, computerMount, client)
	}()
	select {
	case <-mounted:
	case <-time.After(5 * time.Second):
		t.Fatal("Computer Mount did not become ready")
	}
	if err := mounts.RequestFailure(ctx, computerMount.ComputerInstanceID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "failed before start proof") {
			t.Fatalf("server error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Computer Mount owner did not finish Program start failure")
	}
	if rawMachine.closeCount() == 0 {
		t.Fatal("Computer Mount VM was not closed")
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures = %+v", client.failures)
	}
	if got := string(client.failures[0].Error); !strings.Contains(got, "computer_mount_program_start_failed") ||
		strings.Contains(got, "exec image runtime") {
		t.Fatalf("failure error = %s", got)
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("capacity reservations = %d, want 0", got)
	}
}

func TestServerProgramStartFailureKeepsCapacityWhenInstanceCloseFails(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	store, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestChannelCredential = "channel-credential"
	computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
	go acknowledgePreparedComputerMount(
		t,
		preparedServer,
		computerMount,
		computerMount.ComputerInstanceID,
	)
	rawCause := "signed-url-secret-sentinel"
	rawMachine := &serverTestMachine{
		streams:   []io.ReadWriteCloser{preparedClient},
		operation: discardReadWriteCloser{},
		closeErr:  errors.New(rawCause),
	}
	machines := computerPreparedMachines(t, computerMount, rawMachine)
	mounts := NewMounts()
	mounted := make(chan struct{})
	client := &serverTestClient{onReady: func() { close(mounted) }}
	server := Server{RestoreControl: unusedComputerRestoreControl{}, ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{},
		CAS:       store,
		Mounts:    mounts,
		TempDir:   t.TempDir(),
		Heartbeat: time.Hour,
		PollEvery: time.Hour,
		Machines:  machines,
	}
	result := make(chan error, 1)
	go func() {
		result <- server.Serve(ctx, computerMount, client)
	}()
	select {
	case <-mounted:
	case <-time.After(5 * time.Second):
		t.Fatal("Computer Mount did not become ready")
	}
	if err := mounts.RequestFailure(ctx, computerMount.ComputerInstanceID); err == nil ||
		!strings.Contains(err.Error(), rawCause) {
		t.Fatalf("failure request error = %v, want local cleanup cause", err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "computer mount instance cleanup failed") {
			t.Fatalf("server error = %v, want static cleanup failure", err)
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
	if got := len(machines.Reservations.Snapshot().Reservations); got != 1 {
		t.Fatalf("capacity reservations = %d, want 1 until cleanup is proven", got)
	}
	if machines.instanceCheckedOut(computerMount.ComputerInstanceID, computerMount.WorkerEpoch) {
		t.Fatal("close failure must hand checkout to reconciliation")
	}
}

func TestServerRegistersPreparedMachineOverOpenedStream(t *testing.T) {
	ctx := context.Background()
	preparedClient, preparedServer := net.Pipe()
	defer preparedServer.Close()
	_, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestChannelCredential = "channel-credential"
	computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
	machine := &serverTestMachine{
		streams: []io.ReadWriteCloser{preparedClient},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		acknowledgePreparedComputerMount(t, preparedServer, computerMount, "instance-key")
	}()

	err := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).registerComputerMount(ctx, machine, computerMount, "instance-key")
	if err != nil {
		t.Fatal(err)
	}
	<-done
	opened := machine.openedStreams()
	if len(opened) != 1 || opened[0] != preparedClient {
		t.Fatalf("opened streams = %+v, want prepared machine computerMount over OpenStream", opened)
	}
}

func TestServerValidatesSuccessReceiptsOnlyAfterRunningState(t *testing.T) {
	_, computerMount := testComputerMountArtifacts(t)

	computerMount.OrgID = "org-1"
	computerMount.ComputerID = uuid.NewV7().String()
	computerMount.GuestChannelCredential = "channel-credential"
	computerMount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))

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
				return &computerv0.MaterializeComputerResponse{Status: "running", GuestChannelCredentialHash: computerMount.GuestChannelCredentialHash}
			},
			want: "target does not match",
		},
		{
			name: "running without channel receipt",
			response: func(request *computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
				return &computerv0.MaterializeComputerResponse{Status: "running", Target: request.Target}
			},
			want: "guest channel credential hash mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preparedClient, preparedServer := net.Pipe()
			defer preparedServer.Close()
			go respondToPreparedComputerMountWithRequest(t, preparedServer, test.response)
			err := (Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}}).registerComputerMount(context.Background(), &serverTestMachine{
				streams: []io.ReadWriteCloser{preparedClient},
			}, computerMount, "instance-key")
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

func acknowledgePreparedComputerMount(t *testing.T, stream io.ReadWriteCloser, computerMount workerapi.ComputerInstanceAssignment, instanceKey string) {
	t.Helper()
	respondToPreparedComputerMountWithRequest(t, stream, func(request *computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
		if !request.UsePreparedRuntime || request.GetEnvelope().GetComputerInstanceId() != instanceKey {
			t.Errorf("prepared machine request use=%v computer_instance_id=%q", request.UsePreparedRuntime, request.GetEnvelope().GetComputerInstanceId())
		}
		return &computerv0.MaterializeComputerResponse{
			Status:                     "running",
			GuestChannelCredentialHash: computerMount.GuestChannelCredentialHash,
			Target:                     request.Target,
		}
	})
}

type serverTestMachine struct {
	mu        sync.Mutex
	operation io.ReadWriteCloser
	streams   []io.ReadWriteCloser
	opened    []io.ReadWriteCloser
	exit      <-chan error
	closeErr  error
	closed    int
	captures  int
}

func (s *serverTestMachine) Stream() vm.Stream {
	return nil
}

func (s *serverTestMachine) OpenStream(context.Context) (vm.Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed > 0 {
		return nil, errors.New("test machine is closed")
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

func (s *serverTestMachine) Close(context.Context) error {
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

func (s *serverTestMachine) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captures++
	return nil, errTestLiveCapture
}

func (s *serverTestMachine) captureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captures
}

func (s *serverTestMachine) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *serverTestMachine) openedStreams() []io.ReadWriteCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]io.ReadWriteCloser(nil), s.opened...)
}

func (s *serverTestMachine) Wait(ctx context.Context) error {
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

type serverTestClient struct {
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

func (c *serverTestClient) RenewComputerInstance(_ context.Context, request workerapi.ComputerInstanceRenewRequest) (workerapi.ComputerInstanceRenewResponse, error) {
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

func (c *serverTestClient) MarkComputerInstanceClosed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.stops++
	c.closed = append(c.closed, request)
	return workerapi.ComputerInstance{}, nil
}

func (c *serverTestClient) MarkComputerInstanceFailed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.failures = append(c.failures, request)
	if len(c.failErrors) > 0 {
		err := c.failErrors[0]
		c.failErrors = c.failErrors[1:]
		return workerapi.ComputerInstance{}, err
	}
	return workerapi.ComputerInstance{}, nil
}

func (c *serverTestClient) ClaimComputerCommand(_ context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
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

func (c *serverTestClient) CompleteComputerCommand(_ context.Context, request workerapi.ComputerCommandCompleteRequest) error {
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

type serverHTTPError int

func (e serverHTTPError) Error() string {
	return http.StatusText(int(e))
}

func (e serverHTTPError) HTTPStatusCode() int {
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
	server := Server{ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, ArtifactCacheDir: cache}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Go(func() {
			for j := 0; j < 10; j++ {
				path, cleanup, err := server.restoreCASObject(t.Context(), temp, "image", mount.ComputerImage)
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
	conn, guest := net.Pipe()
	defer guest.Close()
	store, mount := testComputerMountArtifacts(t)
	mount.OrgID, mount.ComputerID = "org", uuid.NewV7().String()
	mount.GuestChannelCredential = "channel-credential"
	mount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte("channel-credential"))
	go acknowledgePreparedComputerMount(t, guest, mount, mount.ComputerInstanceID)
	stopErr, reportErr := errors.New("VM stop unproved"), errors.New("failure acknowledgement lost")
	raw := &serverTestMachine{streams: []io.ReadWriteCloser{conn}, operation: discardReadWriteCloser{}, closeErr: stopErr}
	machines := computerPreparedMachines(t, mount, raw)
	mounts := NewMounts()
	mounted := make(chan struct{})
	client := &serverTestClient{onReady: func() { close(mounted) }, failErrors: []error{reportErr}}
	server := Server{RestoreControl: unusedComputerRestoreControl{}, ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, CAS: store, Mounts: mounts, TempDir: t.TempDir(), Heartbeat: time.Hour, PollEvery: time.Hour, Machines: machines}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, mount, client) }()
	select {
	case <-mounted:
	case <-ctx.Done():
		t.Fatal("mount not ready")
	}
	borrowed, err := mounts.OpenChannel(ctx, mount.ComputerInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Channel.Close(context.Background())
	if err := borrowed.ReleaseSource(ctx); !errors.Is(err, stopErr) {
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
	if machines.instanceCheckedOut(mount.ComputerInstanceID, mount.WorkerEpoch) {
		t.Fatal("exited server retained checkout ownership")
	}
	if len(machines.Reservations.Snapshot().Reservations) != 1 {
		t.Fatal("released capacity before physical reclaim")
	}
	if raw.closeCount() != 1 {
		t.Fatal("retried cached machine close instead of deferring physical reclaim")
	}
	// Reconciliation receives a CP-authorized target after active leases expire.
	// It must use host cleanup, not retry the cached machine Close failure.
	connector := &cleanupBackend{err: errors.New("process still alive")}
	machines.Backend = connector
	target := instanceReservationTarget(mount.ComputerInstanceID, mount.WorkerEpoch)
	target.Source.ComputerID = mount.ComputerID
	target.Source.Computer = &workerapi.InstanceComputerSource{VersionID: mount.Target.BaseComputerDiskVersionID}
	control := &typedInstanceClient{}
	if err := machines.reclaimFailedInstanceTarget(ctx, control, target); err == nil {
		t.Fatal("unproved host cleanup succeeded")
	}
	if len(machines.Reservations.Snapshot().Reservations) != 1 || len(control.failed) != 0 {
		t.Fatal("released or published proof before physical cleanup")
	}
	connector.err = nil
	if err := machines.reclaimFailedInstanceTarget(ctx, control, target); err != nil {
		t.Fatal(err)
	}
	if machines.instanceCheckedOut(mount.ComputerInstanceID, mount.WorkerEpoch) || len(machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("retained instance after proved cleanup")
	}
	if len(control.failed) != 1 || control.failed[0].CleanupProof == nil || raw.closeCount() != 1 {
		t.Fatal("reclaim did not publish host proof independently of cached Close")
	}
}

func TestPreparedComputerCheckoutRejectsChangedIdentityWithoutConsuming(t *testing.T) {
	_, mount := testComputerMountArtifacts(t)
	mount.ComputerID = "computer-1"
	machine := &serverTestMachine{}
	machines := computerPreparedMachines(t, mount, machine)
	for _, change := range []func(*workerapi.ComputerInstanceAssignment){func(m *workerapi.ComputerInstanceAssignment) { m.ComputerID = "computer-2" }, func(m *workerapi.ComputerInstanceAssignment) { m.Target.BaseComputerDiskVersionID = "other-version" }} {
		wrong := mount
		change(&wrong)
		if _, _, ok := machines.checkout(t.Context(), wrong); ok {
			t.Fatal("changed Computer source accepted")
		}
	}
	if got, _, ok := machines.checkout(t.Context(), mount); !ok || got.Machine() != machine {
		t.Fatal("valid source was consumed by mismatch")
	}
}

func (c *serverTestClient) AppendCommandLog(_ context.Context, request workerapi.CommandLogAppendRequest) error {
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
	serverTestClient
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
	return c.serverTestClient.ClaimComputerCommand(ctx, r)
}

func TestCommandCompletionLeavesComputerServing(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	physical := &serverTestMachine{operation: host}
	managed := newInstanceMount(physical)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	mount := workerapi.ComputerInstanceAssignment{OrgID: "org-1", ComputerID: "computer-1", ComputerInstanceID: "instance-1", WriterGeneration: 2, GuestChannelCredential: "token"}
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
	client := &continuingCommandClient{serverTestClient: serverTestClient{computerCommand: &command}}
	client.afterCompletion = func() { continued = true; cancel() }
	m := Server{PollEvery: time.Millisecond}
	renewal := m.startRenewalLoop(ctx, workerapi.ComputerInstanceRenewRequest{}, client, time.Hour)
	err := m.serveComputerMount(ctx, renewal, managed, nil, mount, client, nil)
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

func (c *serverTestClient) ReconcileComputerCommand(context.Context, workerapi.ComputerCommandCompleteRequest) error {
	return nil
}

func (*serverTestClient) GetComputerRunCleanup(context.Context, workerapi.ComputerRunCleanupRequest) (workerapi.ComputerRunCleanupResponse, error) {
	return workerapi.ComputerRunCleanupResponse{}, nil
}
func (*serverTestClient) ReconcileComputerRun(context.Context, workerapi.ComputerRunReconcileRequest) error {
	return nil
}
