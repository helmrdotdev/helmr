package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeCheckpointerCreatesManifestAndCleansSnapshotFiles(t *testing.T) {
	stream := newCheckpointStream(t, nil, &runv0.PauseReady{
		WaitpointId:  "waitpoint-1",
		CheckpointId: "checkpoint-1",
	})
	artifact := checkpointArtifact(t)
	session := &checkpointSession{stream: stream, artifact: artifact}
	store := &checkpointCAS{}
	encryptor := testCheckpointEncryptor(t)

	manifest, err := runtimeCheckpointer{
		session:   session,
		cas:       store,
		encryptor: encryptor,
		tempDir:   t.TempDir(),
		stream:    stream,
		workspace: testCheckpointWorkspaceBase(),
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		WaitpointID:  "waitpoint-1",
		CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if session.resumeCount != 0 || len(session.snapshotRequests) != 1 || session.snapshotRequests[0].ID != "checkpoint-1" {
		t.Fatalf("session = %+v", session)
	}
	if stream.closed != 1 {
		t.Fatalf("stream closed %d times", stream.closed)
	}
	assertSuspendFrame(t, stream.written.Bytes(), "waitpoint-1", "checkpoint-1")
	if len(store.puts) != 4 ||
		store.puts[0].mediaType != cas.CheckpointRuntimeConfigMediaType ||
		store.puts[1].mediaType != cas.CheckpointVMStateMediaType ||
		store.puts[2].mediaType != cas.CheckpointScratchDiskMediaType ||
		store.puts[3].mediaType != cas.CheckpointMemoryMediaType {
		t.Fatalf("puts = %+v", store.puts)
	}
	if manifest.RecoveryPoint.Runtime.Backend != "firecracker" || manifest.RecoveryPoint.Runtime.Arch != "arm64" || manifest.RecoveryPoint.Runtime.ABI != "helmr.firecracker.snapshot.v1" {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	if manifest.RecoveryPoint.ID != "checkpoint-1" || manifest.RecoveryPoint.WaitpointID != "waitpoint-1" {
		t.Fatalf("recovery point = %+v", manifest.RecoveryPoint)
	}
	if manifest.RecoveryPoint.Runtime.KernelDigest != "sha256:kernel" || manifest.RecoveryPoint.Runtime.RootfsDigest != "sha256:rootfs" {
		t.Fatalf("manifest digests = %+v", manifest)
	}
	if manifest.RecoveryPoint.Runtime.ConfigDigest != "sha256:runtime-config" {
		t.Fatalf("runtime config digest = %+v", manifest.RecoveryPoint.Runtime.ConfigDigest)
	}
	if artifact := testManifestArtifact(t, manifest, manifest.RuntimeState.ConfigArtifactID); artifact.Digest != store.puts[0].object.Digest {
		t.Fatalf("manifest digest = %+v puts=%+v", artifact, store.puts)
	}
	if artifact := testManifestArtifact(t, manifest, manifest.RuntimeState.VMStateArtifactID); artifact.Digest != store.puts[1].object.Digest {
		t.Fatalf("vm state digest = %+v puts=%+v", artifact, store.puts)
	}
	if artifact := testManifestArtifact(t, manifest, manifest.RuntimeState.ScratchDiskArtifactID); artifact.Digest != store.puts[2].object.Digest {
		t.Fatalf("scratch disk digest = %+v puts=%+v", artifact, store.puts)
	}
	if len(manifest.RuntimeState.MemoryArtifactIDs) != 1 || testManifestArtifact(t, manifest, manifest.RuntimeState.MemoryArtifactIDs[0]).Digest != store.puts[3].object.Digest {
		t.Fatalf("memory artifact ids = %+v puts=%+v", manifest.RuntimeState.MemoryArtifactIDs, store.puts)
	}
	if manifest.WorkspaceState.Base.ArtifactDigest != "sha256:workspace" || manifest.WorkspaceState.Base.MountPath != "/workspace" {
		t.Fatalf("workspace base = %+v", manifest.WorkspaceState.Base)
	}
	if string(manifest.RuntimeState.Config) != `{"runtime":{"backend":"firecracker"}}` {
		t.Fatalf("raw manifest = %s", manifest.RuntimeState.Config)
	}
	assertRemoved(t, artifact.VMState.Path)
	assertRemoved(t, artifact.ScratchDisk.Path)
	assertRemoved(t, artifact.Memory[0].Path)
}

func TestRuntimeCheckpointerProcessesRunEventsBeforePauseReady(t *testing.T) {
	stream := newInterleavedCheckpointStream(t,
		[]proto.Message{&runv0.RunEvent{Event: &runv0.RunEvent_LogEntry{LogEntry: "flushed before checkpoint"}}},
		&runv0.PauseReady{
			WaitpointId:  "waitpoint-1",
			CheckpointId: "checkpoint-1",
		},
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
			events = append(events, event.GetLogEntry())
			return nil
		},
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		WaitpointID:  "waitpoint-1",
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
	stream := newCheckpointStream(t, nil, &runv0.PauseReady{
		WaitpointId:  "other-waitpoint",
		CheckpointId: "checkpoint-1",
	})
	session := &checkpointSession{stream: stream, artifact: checkpointArtifact(t)}

	_, err := runtimeCheckpointer{
		session:   session,
		cas:       &checkpointCAS{},
		encryptor: testCheckpointEncryptor(t),
		tempDir:   t.TempDir(),
		stream:    stream,
	}.CreateCheckpoint(context.Background(), CheckpointRequest{
		WaitpointID:  "waitpoint-1",
		CheckpointID: "checkpoint-1",
	})
	if err == nil || !strings.Contains(err.Error(), `checkpoint pause ready mismatch`) {
		t.Fatalf("err = %v", err)
	}
	if session.resumeCount != 0 || len(session.snapshotRequests) != 0 || stream.closed != 0 {
		t.Fatalf("resumeCount=%d snapshotRequests=%+v closed=%d", session.resumeCount, session.snapshotRequests, stream.closed)
	}
	assertSuspendFrame(t, stream.written.Bytes(), "waitpoint-1", "checkpoint-1")
}

func TestRuntimeCheckpointerResumesOnFailureAfterPause(t *testing.T) {
	tests := []struct {
		name     string
		closeErr error
		snapshot func(t *testing.T) (vm.SnapshotArtifact, error)
		putErrAt int
		want     string
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
			putErrAt: 1,
			want:     "store checkpoint manifest: put failed",
		},
		{
			name: "vm state CAS put",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			putErrAt: 2,
			want:     "store checkpoint vm state: put failed",
		},
		{
			name: "memory CAS put",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			putErrAt: 4,
			want:     "store checkpoint memory: put failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := newCheckpointStream(t, tt.closeErr, &runv0.PauseReady{
				WaitpointId:  "waitpoint-1",
				CheckpointId: "checkpoint-1",
			})
			artifact, snapshotErr := tt.snapshot(t)
			session := &checkpointSession{stream: stream, artifact: artifact, snapshotErr: snapshotErr}

			_, err := runtimeCheckpointer{
				session:   session,
				cas:       &checkpointCAS{putErrAt: tt.putErrAt},
				encryptor: testCheckpointEncryptor(t),
				tempDir:   t.TempDir(),
				stream:    stream,
			}.CreateCheckpoint(context.Background(), CheckpointRequest{
				WaitpointID:  "waitpoint-1",
				CheckpointID: "checkpoint-1",
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if session.resumeCount != 1 {
				t.Fatalf("resumeCount = %d", session.resumeCount)
			}
			if tt.closeErr == nil && stream.closed != 1 {
				t.Fatalf("stream closed %d times", stream.closed)
			}
			assertSuspendFrame(t, stream.written.Bytes(), "waitpoint-1", "checkpoint-1")
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

type checkpointStream struct {
	*scriptedGuestStream
	closeErr error
	closed   int
}

func newCheckpointStream(t *testing.T, closeErr error, messages ...proto.Message) *checkpointStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range messages {
		if ready, ok := message.(*runv0.PauseReady); ok {
			writeCheckpointPauseReadyFrame(t, &read, ready.WaitpointId, ready.CheckpointId)
			continue
		}
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &checkpointStream{scriptedGuestStream: &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}, closeErr: closeErr}
}

func newInterleavedCheckpointStream(t *testing.T, beforeSnapshot []proto.Message, messages ...proto.Message) *checkpointStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range beforeSnapshot {
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	for _, message := range messages {
		if ready, ok := message.(*runv0.PauseReady); ok {
			writeCheckpointPauseReadyFrame(t, &read, ready.WaitpointId, ready.CheckpointId)
			continue
		}
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &checkpointStream{scriptedGuestStream: &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}}
}

func writeCheckpointPauseReadyFrame(t *testing.T, w io.Writer, waitpointID string, checkpointID string) {
	t.Helper()
	if err := transport.WriteStreamFrameHeader(w, transport.StreamHeader{
		Type:         transport.StreamTypeCheckpointPauseReady,
		WaitpointID:  waitpointID,
		CheckpointID: checkpointID,
	}, 0); err != nil {
		t.Fatal(err)
	}
}

func testCheckpointWorkspaceBase() api.WorkerCheckpointWorkspaceBase {
	return api.WorkerCheckpointWorkspaceBase{
		Kind:              "github",
		Repository:        "helmrdotdev/helmr",
		Ref:               "main",
		SHA:               "0123456789abcdef0123456789abcdef01234567",
		ArtifactDigest:    "sha256:workspace",
		ArtifactMediaType: "application/vnd.helmr.workspace.v1.tar",
		ArtifactEncoding:  "tar",
		MountPath:         "/workspace",
		VolumeKind:        "copy-on-write",
	}
}

func (s *checkpointStream) Close() error {
	s.closed += 1
	if s.closeErr != nil {
		return s.closeErr
	}
	return nil
}

type checkpointSession struct {
	stream           io.ReadWriteCloser
	artifact         vm.SnapshotArtifact
	snapshotErr      error
	snapshotRequests []vm.SnapshotRequest
	resumeCount      int
}

func (s *checkpointSession) Stream() io.ReadWriteCloser {
	return s.stream
}

func (s *checkpointSession) Close() error {
	return s.stream.Close()
}

func (s *checkpointSession) CreateSnapshot(_ context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
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
	putErrAt int
	puts     []checkpointCASPut
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
		Digest:    cas.DigestBytes(content),
		SizeBytes: int64(len(content)),
		MediaType: mediaType,
	}
	c.puts = append(c.puts, checkpointCASPut{mediaType: mediaType, content: content, object: object})
	if c.putErrAt > 0 && len(c.puts) == c.putErrAt {
		return cas.Object{}, errors.New("put failed")
	}
	return object, nil
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

func (c *checkpointCAS) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
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
		RuntimeArch:         "arm64",
		RuntimeABI:          "helmr.firecracker.snapshot.v1",
		KernelDigest:        "sha256:kernel",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:runtime-config",
		VMState:             vm.SnapshotFile{Path: state, MediaType: cas.CheckpointVMStateMediaType},
		ScratchDisk:         vm.SnapshotFile{Path: scratch, MediaType: cas.CheckpointScratchDiskMediaType},
		Memory:              []vm.SnapshotFile{{Path: memory, MediaType: cas.CheckpointMemoryMediaType}},
		Manifest:            []byte(`{"runtime":{"backend":"firecracker"}}`),
	}
}

func assertSuspendFrame(t *testing.T, body []byte, waitpointID string, checkpointID string) {
	t.Helper()
	var suspend runv0.SuspendForCheckpoint
	if err := transport.ReadProtoFrame(bytes.NewReader(body), &suspend); err != nil {
		t.Fatal(err)
	}
	if suspend.WaitpointId != waitpointID || suspend.CheckpointId != checkpointID {
		t.Fatalf("suspend = %+v", &suspend)
	}
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s err = %v, want not exist", path, err)
	}
}

func testManifestArtifact(t *testing.T, manifest api.WorkerCheckpointManifest, artifactID string) api.WorkerCheckpointArtifact {
	t.Helper()
	for _, node := range manifest.ArtifactGraph.Artifacts {
		if node.ID == artifactID {
			return node.Artifact
		}
	}
	t.Fatalf("artifact %q not found in %+v", artifactID, manifest.ArtifactGraph.Artifacts)
	return api.WorkerCheckpointArtifact{}
}
