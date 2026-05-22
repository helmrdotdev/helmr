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

	manifest, err := runtimeCheckpointer{
		session:   session,
		cas:       store,
		encryptor: testCheckpointEncryptor(t),
		tempDir:   t.TempDir(),
		stream:    stream,
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
	if len(store.puts) != 2 || store.puts[0].mediaType != cas.CheckpointVMStateMediaType || store.puts[1].mediaType != cas.CheckpointMemoryMediaType {
		t.Fatalf("puts = %+v", store.puts)
	}
	if manifest.RuntimeBackend != "firecracker" || manifest.RuntimeArch != "arm64" || manifest.RuntimeABI != "helmr.firecracker.snapshot.v0" {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	if manifest.KernelDigest == nil || *manifest.KernelDigest != "sha256:kernel" || manifest.RootfsDigest == nil || *manifest.RootfsDigest != "sha256:rootfs" {
		t.Fatalf("manifest digests = %+v", manifest)
	}
	if manifest.RuntimeConfigDigest == nil || *manifest.RuntimeConfigDigest != "sha256:runtime-config" {
		t.Fatalf("runtime config digest = %+v", manifest.RuntimeConfigDigest)
	}
	if manifest.VMStateDigest == nil || *manifest.VMStateDigest != store.puts[0].object.Digest {
		t.Fatalf("vm state digest = %+v puts=%+v", manifest.VMStateDigest, store.puts)
	}
	if len(manifest.MemoryDigests) != 1 || manifest.MemoryDigests[0] != store.puts[1].object.Digest {
		t.Fatalf("memory digests = %+v puts=%+v", manifest.MemoryDigests, store.puts)
	}
	if len(manifest.CASObjects) != 2 || manifest.CASObjects[0].Digest != store.puts[0].object.Digest || manifest.CASObjects[1].Digest != store.puts[1].object.Digest {
		t.Fatalf("CAS objects = %+v puts=%+v", manifest.CASObjects, store.puts)
	}
	if string(manifest.Manifest) != `{"runtime":{"backend":"firecracker"}}` {
		t.Fatalf("raw manifest = %s", manifest.Manifest)
	}
	assertRemoved(t, artifact.VMState.Path)
	assertRemoved(t, artifact.Memory[0].Path)
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
			name: "vm state CAS put",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			putErrAt: 1,
			want:     "store checkpoint vm state: put failed",
		},
		{
			name: "memory CAS put",
			snapshot: func(t *testing.T) (vm.SnapshotArtifact, error) {
				t.Helper()
				return checkpointArtifact(t), nil
			},
			putErrAt: 2,
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
	return &checkpointStream{scriptedGuestStream: newScriptedGuestStream(t, messages...), closeErr: closeErr}
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
	memory := filepath.Join(dir, "memory.mem")
	if err := os.WriteFile(state, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory, []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	return vm.SnapshotArtifact{
		RuntimeBackend:      "firecracker",
		RuntimeArch:         "arm64",
		RuntimeABI:          "helmr.firecracker.snapshot.v0",
		KernelDigest:        "sha256:kernel",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:runtime-config",
		VMState:             vm.SnapshotFile{Path: state, MediaType: cas.CheckpointVMStateMediaType},
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
