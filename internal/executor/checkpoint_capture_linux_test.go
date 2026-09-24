//go:build linux

package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type captureStore struct {
	checkpointCAS
	publish func(cas.Descriptor, *os.File) error
}

func (s *captureStore) Publish(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	if s.publish != nil {
		if err := s.publish(d, f); err != nil {
			return cas.Object{}, err
		}
	}
	return s.checkpointCAS.Publish(ctx, d, f)
}

type retryableCaptureError struct{}

func (retryableCaptureError) Error() string   { return "storage response lost" }
func (retryableCaptureError) Temporary() bool { return true }

func newCaptureTest(t *testing.T) (runtimeCheckpointer, CheckpointRequest, *checkpointSession, *captureStore) {
	t.Helper()
	stream := newCheckpointStream(t, nil, "wait", "checkpoint")
	session := &checkpointSession{stream: stream, artifact: checkpointArtifact(t)}
	store := &captureStore{}
	c := runtimeCheckpointer{publication: testCheckpointPublication, session: session, stream: stream, objects: store, capacity: testCheckpointCapacity(t), encryptor: testCheckpointEncryptor(t), tempDir: t.TempDir()}
	request := CheckpointRequest{RunWaitID: "wait", CheckpointID: "checkpoint", CheckpointRequestVersion: 1, Register: func(context.Context, workerapi.CheckpointManifest) error { return nil }}
	return c, request, session, store
}

func TestCheckpointRegistersAllMembersBeforeRetryingExactCiphertext(t *testing.T) {
	c, request, session, store := newCaptureTest(t)
	var registered workerapi.CheckpointManifest
	request.Register = func(_ context.Context, m workerapi.CheckpointManifest) error {
		if len(store.puts) != 0 {
			t.Fatal("upload before registration")
		}
		if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
			t.Fatal("no reservation during registration")
		}
		registered = m
		return nil
	}
	var attempts []cas.Descriptor
	var firstBytes []byte
	store.publish = func(d cas.Descriptor, f *os.File) error {
		if registered.RuntimeState.Computer == nil {
			t.Fatal("upload without complete registered manifest")
		}
		if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
			t.Fatal("capacity released before upload")
		}
		if session.artifact.Computer.Capture.(*generationCaptureFixture).released {
			t.Fatal("capture released before uploads joined")
		}
		attempts = append(attempts, d)
		data, err := io.ReadAll(io.NewSectionReader(f, 0, d.SizeBytes))
		if err != nil {
			return err
		}
		if len(attempts) == 1 {
			firstBytes = data
			return retryableCaptureError{}
		}
		if len(attempts) == 2 && (!reflect.DeepEqual(firstBytes, data) || attempts[0] != d) {
			t.Fatal("retry changed ciphertext")
		}
		return nil
	}
	session.snapshotHook = func() {
		if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
			t.Fatal("capture before capacity admission")
		}
	}
	result, err := c.CreateCheckpoint(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Manifest, registered) || len(attempts) != 5 || len(store.puts) != 4 {
		t.Fatalf("registration/uploads mismatch: attempts=%d objects=%d", len(attempts), len(store.puts))
	}
	if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
		t.Fatal("staging reservation leaked")
	}
	if !session.artifact.Computer.Capture.(*generationCaptureFixture).released {
		t.Fatal("successful capture retention leaked")
	}
	if session.closeCount != 0 {
		t.Fatal("successful source stopped before ready")
	}

	entries, err := os.ReadDir(c.tempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging survived: %v %v", entries, err)
	}
}

func TestCheckpointCapacityFailurePrecedesPauseAndWrites(t *testing.T) {
	c, request, session, store := newCaptureTest(t)
	c.capacity, _ = capacity.New(capacity.Vector{CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: 1})
	_, err := c.CreateCheckpoint(t.Context(), request)
	if !errors.Is(err, capacity.ErrCapacityExceeded) {
		t.Fatalf("error=%v", err)
	}
	if len(session.snapshotRequests) != 0 || len(store.puts) != 0 || session.stream.(*checkpointStream).written.Len() != 0 {
		t.Fatal("capture or upload before admission")
	}
	entries, _ := os.ReadDir(c.tempDir)
	if len(entries) != 0 {
		t.Fatal("staging created before admission")
	}
}

func TestCheckpointRegistrationFailureStopsBeforeUpload(t *testing.T) {
	c, request, session, store := newCaptureTest(t)
	failure := errors.New("registration rejected")
	request.Register = func(context.Context, workerapi.CheckpointManifest) error { return failure }
	_, err := c.CreateCheckpoint(t.Context(), request)
	if !errors.Is(err, failure) || session.closeCount != 1 || len(store.puts) != 0 {
		t.Fatalf("err=%v closes=%d uploads=%d", err, session.closeCount, len(store.puts))
	}
	if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
		t.Fatal("cleaned candidate retains reservation")
	}
}

func TestCheckpointStopFailureRetainsChargeAndRawSnapshot(t *testing.T) {
	c, request, session, _ := newCaptureTest(t)
	session.closeErr = errors.New("VM exit unproved")
	request.Register = func(context.Context, workerapi.CheckpointManifest) error { return errors.New("registration failed") }
	_, err := c.CreateCheckpoint(t.Context(), request)
	var cleanup *checkpointSourceReleaseError
	if !errors.As(err, &cleanup) || session.closeCount != 1 {
		t.Fatalf("err=%v closes=%d", err, session.closeCount)
	}
	if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
		t.Fatal("unproven cleanup released charge")
	}
	entries, _ := os.ReadDir(c.tempDir)
	if len(entries) != 0 {
		t.Fatal("joined ciphertext staging was not reclaimed")
	}
	if _, err := os.Stat(session.artifact.VMState.Path); err != nil {
		t.Fatalf("unproven source raw state removed: %v", err)
	}
}

func TestCheckpointCleanupFailureAfterUploadStopsSourceAndRetainsCharge(t *testing.T) {
	c, request, session, store := newCaptureTest(t)
	store.publish = func(d cas.Descriptor, f *os.File) error {
		if len(store.puts) == 3 {
			// Replace a completed raw snapshot with a nonempty directory. This creates
			// a real unlink failure after all uploads, without mocking the cleanup path.
			p := session.artifact.VMState.Path
			if err := os.Remove(p); err != nil {
				return err
			}
			if err := os.Mkdir(p, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(p, "busy"), []byte("retain"), 0600)
		}
		return nil
	}
	_, err := c.CreateCheckpoint(t.Context(), request)
	var cleanup *checkpointSourceReleaseError
	if !errors.As(err, &cleanup) || session.closeCount != 1 || len(store.puts) != 4 {
		t.Fatalf("err=%v closes=%d uploads=%d", err, session.closeCount, len(store.puts))
	}
	if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
		t.Fatal("failed cleanup released charge")
	}
}

func TestCheckpointDuplicateCaptureDoesNotStopExistingOwner(t *testing.T) {
	c, request, session, _ := newCaptureTest(t)
	shape, _ := session.SnapshotLimits()
	limits, err := checkpointStagingSize(shape, c.encryptor)
	if err != nil {
		t.Fatal(err)
	}
	key := capacity.Key{Kind: "checkpoint-staging", ID: request.CheckpointID, Epoch: request.CheckpointRequestVersion}
	if _, err := c.capacity.Reserve(key, capacity.Vector{GuestEphemeralDiskBytes: limits.total}); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateCheckpoint(t.Context(), request)
	if err == nil || session.closeCount != 0 || len(session.snapshotRequests) != 0 {
		t.Fatalf("err=%v closes=%d", err, session.closeCount)
	}
	if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes != limits.total {
		t.Fatal("existing owner reservation changed")
	}
}
