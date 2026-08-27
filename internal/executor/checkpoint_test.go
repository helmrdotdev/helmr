package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeCheckpointerCreatesManifestAndCleansSnapshotFiles(t *testing.T) {
	stream := newCheckpointStream(t, nil, "run-wait-id-1", "checkpoint-1")
	artifact := checkpointArtifact(t)
	addCheckpointRuntimeSubstrate(t, &artifact)
	session := &checkpointSession{stream: stream, artifact: artifact}
	store := &checkpointCAS{}
	encryptor := testCheckpointEncryptor(t)

	result, err := runtimeCheckpointer{
		session:   session,
		cas:       store,
		encryptor: encryptor,
		tempDir:   t.TempDir(),
		stream:    stream,
		workspace: testCheckpointWorkspaceBase(),
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := result.Manifest

	if session.resumeCount != 0 || session.closeCount != 0 || len(session.snapshotRequests) != 1 || session.snapshotRequests[0].ID != "checkpoint-1" {
		t.Fatalf("session = %+v", session)
	}
	if err := (runtimeCheckpointer{session: session}).ReleaseCheckpointSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if session.closeCount != 1 {
		t.Fatalf("session close count = %d, want 1", session.closeCount)
	}
	if stream.closed != 1 {
		t.Fatalf("stream closed %d times", stream.closed)
	}
	assertSuspendFrame(t, stream.written.Bytes(), "run-wait-id-1", "checkpoint-1")
	if len(store.puts) != 4 {
		t.Fatalf("puts = %+v", store.puts)
	}
	manifestPut := checkpointPutByMediaType(t, store, cas.CheckpointRuntimeConfigMediaType)
	vmStatePut := checkpointPutByMediaType(t, store, cas.CheckpointVMStateMediaType)
	scratchPut := checkpointPutByMediaType(t, store, cas.CheckpointScratchDiskMediaType)
	memoryPut := checkpointPutByMediaType(t, store, cas.CheckpointMemoryMediaType)
	if manifest.RecoveryPoint.Runtime.Backend != "firecracker" || manifest.RecoveryPoint.Runtime.Arch != "x86_64" || manifest.RecoveryPoint.Runtime.Contract != "helmr.vm-runtime.v0" {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	if manifest.RecoveryPoint.ID != "checkpoint-1" || manifest.RecoveryPoint.RunWaitID != "run-wait-id-1" {
		t.Fatalf("recovery point = %+v", manifest.RecoveryPoint)
	}
	if manifest.RecoveryPoint.Runtime.KernelDigest != "sha256:kernel" || manifest.RecoveryPoint.Runtime.RootfsDigest != "sha256:rootfs" {
		t.Fatalf("manifest digests = %+v", manifest)
	}
	if manifest.RecoveryPoint.Runtime.ConfigDigest != "sha256:runtime-config" {
		t.Fatalf("runtime config digest = %+v", manifest.RecoveryPoint.Runtime.ConfigDigest)
	}
	if manifest.RecoveryPoint.Runtime.VMVCPUCount != 2 ||
		manifest.RecoveryPoint.Runtime.CPUConfigDigest != sha256sum.DigestBytes([]byte("cpu-config")) {
		t.Fatalf("runtime CPU shape = %+v", manifest.RecoveryPoint.Runtime)
	}
	if manifest.RecoveryPoint.Runtime.Substrate == nil || manifest.RecoveryPoint.Runtime.Substrate.Digest != sha256sum.DigestBytes([]byte("substrate")) {
		t.Fatalf("runtime substrate = %+v", manifest.RecoveryPoint.Runtime.Substrate)
	}
	if manifest.RuntimeState.ConfigArtifact.Digest != manifestPut.object.Digest {
		t.Fatalf("manifest artifact = %+v puts=%+v", manifest.RuntimeState.ConfigArtifact, store.puts)
	}
	if manifest.RuntimeState.VMStateArtifact.Digest != vmStatePut.object.Digest {
		t.Fatalf("vm state artifact = %+v puts=%+v", manifest.RuntimeState.VMStateArtifact, store.puts)
	}
	if manifest.RuntimeState.ScratchDiskArtifact.Digest != scratchPut.object.Digest {
		t.Fatalf("scratch disk artifact = %+v puts=%+v", manifest.RuntimeState.ScratchDiskArtifact, store.puts)
	}
	if len(manifest.RuntimeState.MemoryArtifacts) != 1 || manifest.RuntimeState.MemoryArtifacts[0].Digest != memoryPut.object.Digest {
		t.Fatalf("memory artifacts = %+v puts=%+v", manifest.RuntimeState.MemoryArtifacts, store.puts)
	}
	if manifest.WorkspaceState.Base.ArtifactDigest != "sha256:workspace" || manifest.WorkspaceState.Base.MountPath != "/workspace" {
		t.Fatalf("workspace base = %+v", manifest.WorkspaceState.Base)
	}
	if string(manifest.RuntimeState.Config) != `{"runtime":{"backend":"firecracker"}}` {
		t.Fatalf("raw manifest = %s", manifest.RuntimeState.Config)
	}
	if !checkpointPhaseHasFilepackStats(manifest.Phases, "pack_scratch_filepack") {
		t.Fatalf("manifest phases missing scratch filepack stats: %+v", manifest.Phases)
	}
	assertRemoved(t, artifact.VMState.Path)
	assertRemoved(t, artifact.ScratchDisk.Path)
	if _, err := os.Stat(artifact.Substrate.Path); err != nil {
		t.Fatalf("session-owned substrate projection was removed by checkpoint cleanup: %v", err)
	}
	assertRemoved(t, artifact.Memory[0].Path)
}

func TestRuntimeCheckpointerPublishesFrozenAuthorityBeforeSnapshot(t *testing.T) {
	stream := newCheckpointStream(t, nil, "run-wait-id-1", "checkpoint-1")
	artifact := checkpointArtifact(t)
	addCheckpointRuntimeSubstrate(t, &artifact)
	frozen := false
	session := &checkpointSession{
		stream: stream, artifact: artifact,
		snapshotHook: func() {
			if !frozen {
				t.Error("snapshot began before the frozen authority transition")
			}
		},
	}
	_, err := runtimeCheckpointer{
		session: session, cas: &checkpointCAS{}, encryptor: testCheckpointEncryptor(t),
		tempDir: t.TempDir(), stream: stream, workspace: testCheckpointWorkspaceBase(),
		onFrozen: func() { frozen = true },
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !frozen {
		t.Fatal("frozen authority transition was not published")
	}
}

func TestValidateRestoreIdentityRejectsMissingSubstrateSize(t *testing.T) {
	checkpoint := workerapi.CheckpointManifest{RecoveryPoint: workerapi.CheckpointRecoveryPoint{
		Runtime: workerapi.CheckpointRuntime{
			Backend: "firecracker", Arch: string(deployment.ArchitectureX8664),
			Contract: "helmr.vm-runtime.v0", ID: "sha256:runtime",
			KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs",
			RootfsDigest: "sha256:rootfs", ConfigDigest: "sha256:config",
			VMVCPUCount: 2, CPUConfigDigest: sha256sum.DigestBytes([]byte("cpu-config")),
			Substrate: &workerapi.CheckpointRuntimeSubstrate{
				Digest: "sha256:substrate", Format: "ext4", Contract: "helmr.substrate.v0",
			},
		},
	}}
	if err := validateRestoreIdentity(checkpoint, deployment.ArchitectureX8664); err == nil ||
		!strings.Contains(err.Error(), "substrate.size_bytes must be positive") {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeCheckpointerClosesSourceOnPrePauseConfigurationFailure(t *testing.T) {
	stream := newCheckpointStream(t, nil, "", "")
	session := &checkpointSession{stream: stream}
	_, err := runtimeCheckpointer{session: session, stream: stream}.CreateCheckpoint(
		context.Background(),
		CheckpointRequest{RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1"},
	)
	if err == nil || !strings.Contains(err.Error(), "checkpoint CAS is required") {
		t.Fatalf("err = %v", err)
	}
	if session.closeCount != 1 || stream.closed != 1 || session.resumeCount != 0 {
		t.Fatalf("session close/resume = %d/%d, stream closed = %d", session.closeCount, session.resumeCount, stream.closed)
	}
}

func TestRuntimeCheckpointerSeparatesWorkspaceCaptureFromRuntimeManifest(t *testing.T) {
	var read bytes.Buffer
	workspaceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "result.txt"), []byte("workspace result"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceArtifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(workspaceRoot, t.TempDir(), workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	workspaceBody, err := os.ReadFile(workspaceArtifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	workspaceDigest := sha256sum.DigestBytes(workspaceBody)
	entryCount := workspaceArtifact.EntryCount
	if err := wire.WriteStreamFrameHeader(&read, wire.StreamHeader{
		Type:       wire.StreamTypeWorkspaceArtifact,
		RunID:      "run-1",
		BodyDigest: &workspaceDigest,
		EntryCount: &entryCount,
	}, uint64(len(workspaceBody))); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Write(workspaceBody); err != nil {
		t.Fatal(err)
	}
	writeCheckpointPauseReadyFrame(t, &read, "run-wait-id-1", "checkpoint-1")
	stream := &checkpointStream{scriptedGuestStream: &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}}
	artifact := checkpointArtifact(t)
	session := &checkpointSession{stream: stream, artifact: artifact}
	store := &checkpointCAS{}
	result, err := runtimeCheckpointer{
		session:   session,
		cas:       store,
		encryptor: testCheckpointEncryptor(t),
		tempDir:   t.TempDir(),
		stream:    stream,
		workspace: testCheckpointWorkspaceBase(),
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		RunID:            "run-1",
		RunWaitID:        "run-wait-id-1",
		CheckpointID:     "checkpoint-1",
		CaptureWorkspace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkspaceCapture == nil || result.WorkspaceCapture.Artifact.Digest != workspaceDigest || result.WorkspaceCapture.Artifact.EntryCount != entryCount || result.WorkspaceCapture.Tree.EntryCount != entryCount {
		t.Fatalf("workspace capture = %+v, want digest=%s entries=%d", result.WorkspaceCapture, workspaceDigest, entryCount)
	}
	if result.Manifest.WorkspaceState.Base.ArtifactDigest != "sha256:workspace" {
		t.Fatalf("manifest workspace base = %+v, want original base only", result.Manifest.WorkspaceState.Base)
	}
	workspacePut := checkpointPutByMediaType(t, store, workspace.ArtifactMediaType)
	if string(workspacePut.content) != string(workspaceBody) {
		t.Fatalf("workspace capture body = %q, want %q", workspacePut.content, workspaceBody)
	}
}

func TestRuntimeCheckpointerProcessesRunEventsBeforePauseReady(t *testing.T) {
	stream := newInterleavedCheckpointStream(t,
		[]proto.Message{&runv0.RunEvent{Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("flushed before checkpoint")}}},
		"run-wait-id-1", "checkpoint-1",
	)
	artifact := checkpointArtifact(t)
	session := &checkpointSession{stream: stream, artifact: artifact}
	store := &checkpointCAS{}
	encryptor := testCheckpointEncryptor(t)
	var events []string

	_, err := runtimeCheckpointer{
		session:   session,
		cas:       store,
		encryptor: encryptor,
		tempDir:   t.TempDir(),
		stream:    stream,
		workspace: testCheckpointWorkspaceBase(),
		runEvent: func(_ context.Context, event *runv0.RunEvent) error {
			events = append(events, string(event.GetStdoutChunk()))
			return nil
		},
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0] != "flushed before checkpoint" {
		t.Fatalf("events = %+v", events)
	}
	if len(session.snapshotRequests) != 1 {
		t.Fatalf("snapshotRequests = %+v", session.snapshotRequests)
	}
}

func TestRuntimeCheckpointerRejectsPauseReadyMismatch(t *testing.T) {
	stream := newCheckpointStream(t, nil, "other-run wait", "checkpoint-1")
	session := &checkpointSession{stream: stream, artifact: checkpointArtifact(t)}

	_, err := runtimeCheckpointer{
		session:   session,
		cas:       &checkpointCAS{},
		encryptor: testCheckpointEncryptor(t),
		tempDir:   t.TempDir(),
		stream:    stream,
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	})
	if err == nil || !strings.Contains(err.Error(), `checkpoint pause ready mismatch`) {
		t.Fatalf("err = %v", err)
	}
	if session.resumeCount != 0 || session.closeCount != 1 || len(session.snapshotRequests) != 0 || stream.closed != 1 {
		t.Fatalf("resumeCount=%d closeCount=%d snapshotRequests=%+v closed=%d", session.resumeCount, session.closeCount, session.snapshotRequests, stream.closed)
	}
	assertSuspendFrame(t, stream.written.Bytes(), "run-wait-id-1", "checkpoint-1")
}

func TestRuntimeCheckpointerRejectsPauseReadyBody(t *testing.T) {
	var read bytes.Buffer
	if err := wire.WriteStreamFrameHeader(&read, wire.StreamHeader{
		Type:         wire.StreamTypeCheckpointPauseReady,
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	}, 1); err != nil {
		t.Fatal(err)
	}
	read.WriteByte(0)

	_, err := (runtimeCheckpointer{}).readPauseReady(context.Background(), bufio.NewReader(&read), CheckpointRequest{
		RunWaitID: "run-wait-id-1", CheckpointID: "checkpoint-1",
	})
	if err == nil || !strings.Contains(err.Error(), "body length must be zero") {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeCheckpointerPauseReadyTimeoutDoesNotCloseSession(t *testing.T) {
	stream := newBlockingGuestStream()
	session := &checkpointSession{stream: stream}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runtimeCheckpointer{
		session: session,
		stream:  stream,
	}.readPauseReadyContext(ctx, bufio.NewReader(stream), CheckpointRequest{
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if session.closeCount != 0 {
		t.Fatalf("session close count = %d, want 0", session.closeCount)
	}
	if !stream.isClosed() {
		t.Fatal("checkpoint stream was not closed")
	}
}

func TestRuntimeCheckpointerClosesSourceOnFailureAfterPause(t *testing.T) {
	tests := []struct {
		name            string
		closeErr        error
		snapshot        func(t *testing.T) (vm.SnapshotArtifact, error)
		putErrMediaType string
		want            string
	}{
		{
			name:     "control stream close",
			closeErr: errors.New("close failed"),
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			want: "close checkpoint control stream: close failed",
		},
		{
			name: "snapshot",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return vm.SnapshotArtifact{}, errors.New("snapshot failed")
			},
			want: "snapshot failed",
		},
		{
			name: "manifest CAS put",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			putErrMediaType: cas.CheckpointRuntimeConfigMediaType,
			want:            "store checkpoint manifest: put failed",
		},
		{
			name: "vm state CAS put",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			putErrMediaType: cas.CheckpointVMStateMediaType,
			want:            "store checkpoint vm state: put failed",
		},
		{
			name: "memory CAS put",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			putErrMediaType: cas.CheckpointMemoryMediaType,
			want:            "store checkpoint memory: put failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := newCheckpointStream(t, tt.closeErr, "run-wait-id-1", "checkpoint-1")
			artifact, snapshotErr := tt.snapshot(t)
			session := &checkpointSession{stream: stream, artifact: artifact, snapshotErr: snapshotErr}
			_, err := runtimeCheckpointer{
				session:   session,
				cas:       &checkpointCAS{putErrMediaType: tt.putErrMediaType},
				encryptor: testCheckpointEncryptor(t),
				tempDir:   t.TempDir(),
				stream:    stream,
			}.CreateCheckpoint(context.Background(), CheckpointRequest{
				RunWaitID:    "run-wait-id-1",
				CheckpointID: "checkpoint-1",
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if session.resumeCount != 0 || session.closeCount != 1 {
				t.Fatalf("resumeCount = %d, closeCount = %d, want 0/1", session.resumeCount, session.closeCount)
			}
			if tt.closeErr == nil && stream.closed != 1 {
				t.Fatalf("stream closed %d times", stream.closed)
			}
			assertSuspendFrame(t, stream.written.Bytes(), "run-wait-id-1", "checkpoint-1")
			if len(session.snapshotRequests) > 0 && artifact.VMState.Path != "" {
				assertRemoved(t, artifact.VMState.Path)
			}
			if len(session.snapshotRequests) > 0 {
				for _, memory := range artifact.Memory {
					assertRemoved(t, memory.Path)
				}
			}
		})
	}
}

func TestRuntimeCheckpointerReleaseBorrowedSourceDoesNotCloseControlStreamTwice(t *testing.T) {
	stream := &nonIdempotentCheckpointStream{
		checkpointStream: newCheckpointStream(t, nil, "run-wait-id-1", "checkpoint-1"),
	}
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}}
	parent.artifact = checkpointArtifact(t)
	session, ok := newBorrowedRunSession(parent, testVMStream(stream)).(vm.CheckpointableSession)
	if !ok {
		t.Fatal("borrowed session is not checkpointable")
	}

	checkpointer := runtimeCheckpointer{
		session:   session,
		cas:       &checkpointCAS{},
		encryptor: testCheckpointEncryptor(t),
		tempDir:   t.TempDir(),
		stream:    stream,
	}
	_, err := checkpointer.CreateCheckpoint(context.Background(), CheckpointRequest{
		RunWaitID:    "run-wait-id-1",
		CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}
	if stream.closed != 1 {
		t.Fatalf("stream closed %d times, want 1", stream.closed)
	}
	if err := checkpointer.ReleaseCheckpointSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if parent.closeCount != 1 {
		t.Fatalf("parent close count = %d, want 1", parent.closeCount)
	}
}

type checkpointStream struct {
	*scriptedGuestStream
	closeErr error
	closed   int
}

func newCheckpointStream(t *testing.T, closeErr error, runWaitID string, checkpointID string, messages ...proto.Message) *checkpointStream {
	t.Helper()
	var read bytes.Buffer
	if runWaitID != "" || checkpointID != "" {
		writeCheckpointPauseReadyFrame(t, &read, runWaitID, checkpointID)
	}
	for _, message := range messages {
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := frameio.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &checkpointStream{scriptedGuestStream: &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}, closeErr: closeErr}
}

func newInterleavedCheckpointStream(t *testing.T, beforeSnapshot []proto.Message, runWaitID string, checkpointID string, messages ...proto.Message) *checkpointStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range beforeSnapshot {
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := frameio.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	writeCheckpointPauseReadyFrame(t, &read, runWaitID, checkpointID)
	for _, message := range messages {
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := frameio.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &checkpointStream{scriptedGuestStream: &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}}
}

func writeCheckpointPauseReadyFrame(t *testing.T, w io.Writer, runWaitID string, checkpointID string) {
	t.Helper()
	if err := wire.WriteStreamFrameHeader(w, wire.StreamHeader{
		Type:         wire.StreamTypeCheckpointPauseReady,
		RunWaitID:    runWaitID,
		CheckpointID: checkpointID,
	}, 0); err != nil {
		t.Fatal(err)
	}
}

func testCheckpointWorkspaceBase() workerapi.CheckpointWorkspaceBase {
	return workerapi.CheckpointWorkspaceBase{
		ArtifactDigest:    "sha256:workspace",
		ArtifactMediaType: "application/vnd.helmr.workspace.v0.tar",
		ArtifactEncoding:  "tar",
		MountPath:         "/workspace",
	}
}

func (s *checkpointStream) Close() error {
	if s.closed > 0 {
		return nil
	}
	s.closed += 1
	if s.closeErr != nil {
		return s.closeErr
	}
	return nil
}

type nonIdempotentCheckpointStream struct {
	*checkpointStream
}

func (s *nonIdempotentCheckpointStream) Close() error {
	if s.closed > 0 {
		return errors.New("control stream closed twice")
	}
	return s.checkpointStream.Close()
}

type checkpointSession struct {
	stream           io.ReadWriteCloser
	artifact         vm.SnapshotArtifact
	snapshotErr      error
	snapshotRequests []vm.SnapshotRequest
	resumeCount      int
	closeCount       int
	closeErr         error
	closed           bool
	snapshotHook     func()
}

func (s *checkpointSession) Stream() vm.Stream {
	return testVMStream(s.stream)
}

func (s *checkpointSession) OpenStream(context.Context) (vm.Stream, error) {
	return testVMStream(s.stream), nil
}

func (s *checkpointSession) Close(context.Context) error {
	s.closeCount += 1
	if s.closeErr != nil {
		return s.closeErr
	}
	if s.closed {
		return nil
	}
	s.closed = true
	return s.stream.Close()
}

func (s *checkpointSession) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *checkpointSession) CreateSnapshot(_ context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	if s.snapshotHook != nil {
		s.snapshotHook()
	}
	s.snapshotRequests = append(s.snapshotRequests, request)
	if s.snapshotErr != nil {
		return vm.SnapshotArtifact{}, s.snapshotErr
	}
	return s.artifact, nil
}

func (s *checkpointSession) Resume(context.Context) error {
	s.resumeCount += 1
	return nil
}

type checkpointCAS struct {
	mu              sync.Mutex
	putErrMediaType string
	puts            []checkpointCASPut
}

type checkpointCASPut struct {
	mediaType string
	content   []byte
	object    cas.Object
}

func (c *checkpointCAS) Put(_ context.Context, mediaType string, body io.Reader) (cas.Object, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return cas.Object{}, err
	}
	return c.put(mediaType, content)
}

func (c *checkpointCAS) Stage(_ context.Context, mediaType string) (cas.Stage, error) {
	return &checkpointCASStage{store: c, mediaType: mediaType}, nil
}

func (c *checkpointCAS) put(mediaType string, content []byte) (cas.Object, error) {
	object := cas.Object{
		Digest:    sha256sum.DigestBytes(content),
		SizeBytes: int64(len(content)),
		MediaType: mediaType,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts = append(c.puts, checkpointCASPut{mediaType: mediaType, content: content, object: object})
	if c.putErrMediaType != "" && mediaType == c.putErrMediaType {
		return cas.Object{}, errors.New("put failed")
	}
	return object, nil
}

func checkpointPutByMediaType(t *testing.T, store *checkpointCAS, mediaType string) checkpointCASPut {
	t.Helper()
	for _, put := range store.puts {
		if put.mediaType == mediaType {
			return put
		}
	}
	t.Fatalf("missing checkpoint CAS put for media type %q: %+v", mediaType, store.puts)
	return checkpointCASPut{}
}

type checkpointCASStage struct {
	store     *checkpointCAS
	mediaType string
	content   bytes.Buffer
	closed    bool
}

func (s *checkpointCASStage) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("stage is closed")
	}
	return s.content.Write(p)
}

func (s *checkpointCASStage) Close() error {
	s.closed = true
	return nil
}

func (s *checkpointCASStage) Commit(context.Context) (cas.Object, error) {
	s.closed = true
	return s.store.put(s.mediaType, s.content.Bytes())
}

func (s *checkpointCASStage) Abort(context.Context) error {
	s.closed = true
	return nil
}

func (c *checkpointCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, nil
}

func (c *checkpointCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, put := range c.puts {
		if put.object.Digest == digest {
			return io.NopCloser(bytes.NewReader(append([]byte(nil), put.content...))), nil
		}
	}
	return nil, errors.New("not found")
}

func (c *checkpointCAS) Delete(context.Context, string) error {
	return nil
}

func checkpointArtifact(t *testing.T) vm.SnapshotArtifact {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state.vmstate")
	scratch := filepath.Join(dir, "scratch.ext4")
	memory := filepath.Join(dir, "memory.mem")
	if err := os.WriteFile(state, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory, []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scratch, []byte("scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	return vm.SnapshotArtifact{
		RuntimeBackend:      "firecracker",
		RuntimeID:           "sha256:runtime",
		RuntimeArch:         "x86_64",
		VMRuntimeContract:   "helmr.vm-runtime.v0",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:runtime-config",
		VMVCPUCount:         2,
		CPUConfigDigest:     sha256sum.DigestBytes([]byte("cpu-config")),
		VMState:             vm.SnapshotFile{Path: state, MediaType: cas.CheckpointVMStateMediaType},
		ScratchDisk: vm.SnapshotFile{Path: scratch, MediaType: cas.CheckpointScratchDiskMediaType, Filepack: &vm.FilepackStats{
			LogicalBytes:  1024,
			EncodedChunks: 1,
		}},
		Memory: []vm.SnapshotFile{{Path: memory, MediaType: cas.CheckpointMemoryMediaType}},
		Phases: []vm.RuntimePhase{{
			Name:      "pack_scratch_filepack",
			Role:      "scratch-disk",
			MediaType: cas.CheckpointScratchDiskMediaType,
			Filepack: &vm.FilepackStats{
				LogicalBytes:  1024,
				EncodedChunks: 1,
			},
		}},
		Manifest: []byte(`{"runtime":{"backend":"firecracker"}}`),
	}
}

func testCheckpointRuntimeArchitecture() string {
	return string(deployment.ArchitectureX8664)
}

func addCheckpointRuntimeSubstrate(t *testing.T, artifact *vm.SnapshotArtifact) {
	t.Helper()
	path := filepath.Join(filepath.Dir(artifact.VMState.Path), "substrate.ext4")
	if err := os.WriteFile(path, []byte("substrate"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact.Substrate = &vm.RuntimeSubstrate{
		Path:     path,
		Digest:   sha256sum.DigestBytes([]byte("substrate")),
		Format:   "ext4",
		Contract: "helmr.runtime-substrate.builder.v0",
	}
}

func checkpointPhaseHasFilepackStats(phases []workerapi.CheckpointPhase, name string) bool {
	for _, phase := range phases {
		if phase.Name == name && phase.Filepack != nil && phase.Filepack.LogicalBytes > 0 {
			return true
		}
	}
	return false
}

func assertSuspendFrame(t *testing.T, body []byte, runWaitID string, checkpointID string) {
	t.Helper()
	reader := bytes.NewReader(body)
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	suspend, err := wire.ReadCheckpointPauseRequest(header, reader, bodyLen)
	if err != nil {
		t.Fatal(err)
	}
	if suspend.RunWaitId != runWaitID || suspend.CheckpointId != checkpointID {
		t.Fatalf("suspend = %+v", suspend)
	}
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s err = %v, want not exist", path, err)
	}
}
