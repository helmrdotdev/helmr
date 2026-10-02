package computerhost

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/reservation"
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

func newCaptureTest(t *testing.T) (computerCheckpointer, computerCheckpointRequest, *checkpointMachine, *captureStore) {
	t.Helper()
	target := checkpointCaptureTarget(2)
	stream := checkpointFreezeStream(t, target)
	machine := &checkpointMachine{stream: stream, artifact: checkpointArtifact(t)}
	store := &captureStore{}
	c := computerCheckpointer{publication: testCheckpointPublication, machine: machine, objects: store, reservations: testCheckpointReservations(t), encryptor: testCheckpointEncryptor(t), tempDir: t.TempDir()}
	request := computerCheckpointRequest{Target: target, Register: func(context.Context, workerapi.CheckpointManifest) error { return nil }}
	return c, request, machine, store
}

func TestCheckpointRegistersAllMembersBeforeRetryingExactCiphertext(t *testing.T) {
	c, request, machine, store := newCaptureTest(t)
	var registered workerapi.CheckpointManifest
	request.Register = func(_ context.Context, m workerapi.CheckpointManifest) error {
		if len(store.puts) != 0 {
			t.Fatal("upload before registration")
		}
		if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
			t.Fatal("no reservation during registration")
		}
		if m.RecoveryPoint.ComputerID != request.Target.Source.ComputerID || m.RecoveryPoint.ComputerInstanceID != request.Target.ID || m.RecoveryPoint.WriterGeneration != request.Target.Source.WriterGeneration || m.RecoveryPoint.MembershipRevision != request.Target.Capture.MembershipRevision || len(m.RecoveryPoint.Runs) != 2 {
			t.Fatalf("incomplete physical membership: %+v", m.RecoveryPoint)
		}
		for i, member := range m.RecoveryPoint.Runs {
			expected := request.Target.Capture.Runs[i]
			if member.RunID != expected.RunID || member.RunLeaseID != expected.RunLeaseID || member.RunWaitID != expected.RunWaitID || member.CorrelationID != "correlation-"+expected.RunID || !reflect.DeepEqual(member.SessionSpeculativeInputSequence, expected.SessionSpeculativeInputSequence) {
				t.Fatalf("changed member: %+v", member)
			}
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
		if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
			t.Fatal("capacity released before upload")
		}
		if machine.artifact.Computer.Capture.(*versionCaptureFixture).released {
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
	machine.snapshotHook = func() {
		if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
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
	if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
		t.Fatal("staging reservation leaked")
	}
	if !machine.artifact.Computer.Capture.(*versionCaptureFixture).released {
		t.Fatal("successful capture retention leaked")
	}
	if machine.closeCount != 0 {
		t.Fatal("successful source stopped before ready")
	}

	entries, err := os.ReadDir(c.tempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging survived: %v %v", entries, err)
	}
}

func TestCheckpointCapacityFailurePrecedesPauseAndWrites(t *testing.T) {
	c, request, machine, store := newCaptureTest(t)
	c.reservations, _ = reservation.New(reservation.Vector{CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: 1})
	_, err := c.CreateCheckpoint(t.Context(), request)
	if !errors.Is(err, reservation.ErrCapacityExceeded) {
		t.Fatalf("error=%v", err)
	}
	if len(machine.snapshotRequests) != 0 || len(store.puts) != 0 || machine.stream.(*checkpointStream).written.Len() != 0 {
		t.Fatal("capture or upload before admission")
	}
	entries, _ := os.ReadDir(c.tempDir)
	if len(entries) != 0 {
		t.Fatal("staging created before admission")
	}
}

func TestCheckpointRegistrationFailureRetainsSourceBeforeUpload(t *testing.T) {
	c, request, machine, store := newCaptureTest(t)
	failure := errors.New("registration rejected")
	request.Register = func(context.Context, workerapi.CheckpointManifest) error { return failure }
	_, err := c.CreateCheckpoint(t.Context(), request)
	if !errors.Is(err, failure) || machine.closeCount != 0 || len(store.puts) != 0 {
		t.Fatalf("err=%v closes=%d uploads=%d", err, machine.closeCount, len(store.puts))
	}
	if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 || c.pendingCleanup == nil {
		t.Fatal("failed candidate lost its retained staging owner")
	}
}

func TestCheckpointStopFailureRetainsChargeAndRawSnapshot(t *testing.T) {
	c, request, machine, _ := newCaptureTest(t)
	machine.closeErr = errors.New("VM exit unproved")
	request.Register = func(context.Context, workerapi.CheckpointManifest) error { return errors.New("registration failed") }
	_, err := c.CreateCheckpoint(t.Context(), request)
	if err == nil || machine.closeCount != 0 {
		t.Fatalf("capture closed source: %v", err)
	}
	if err = c.ReleaseCheckpointSource(t.Context()); !errors.Is(err, machine.closeErr) || machine.closeCount != 1 {
		t.Fatalf("terminal close: %v count=%d", err, machine.closeCount)
	}
	if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
		t.Fatal("unproven cleanup released charge")
	}
	entries, _ := os.ReadDir(c.tempDir)
	if len(entries) == 0 {
		t.Fatal("failed source release discarded retained staging")
	}
	machine.closeErr = nil
	t.Cleanup(func() {
		if err := c.ReleaseCheckpointSource(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.Stat(machine.artifact.VMState.Path); err != nil {
		t.Fatalf("unproven source raw state removed: %v", err)
	}
}

func TestCheckpointCleanupFailureAfterUploadRetainsSourceAndCharge(t *testing.T) {
	c, request, machine, store := newCaptureTest(t)
	store.publish = func(d cas.Descriptor, f *os.File) error {
		if len(store.puts) == 3 {
			// Replace a completed raw snapshot with a nonempty directory. This creates
			// a real unlink failure after all uploads, without mocking the cleanup path.
			p := machine.artifact.VMState.Path
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
	if err == nil || machine.closeCount != 0 || len(store.puts) != 4 {
		t.Fatalf("err=%v closes=%d uploads=%d", err, machine.closeCount, len(store.puts))
	}
	if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
		t.Fatal("failed cleanup released charge")
	}
}

func TestCheckpointDuplicateCaptureDoesNotStopExistingOwner(t *testing.T) {
	c, request, machine, _ := newCaptureTest(t)
	shape, _ := machine.SnapshotLimits()
	limits, err := checkpointStagingSize(shape, c.encryptor)
	if err != nil {
		t.Fatal(err)
	}
	key := reservation.Key{Kind: "checkpoint-staging", ID: request.Target.Capture.CheckpointID, Epoch: request.Target.DesiredVersion}
	if _, err := c.reservations.Reserve(key, reservation.Vector{GuestEphemeralDiskBytes: limits.total}); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateCheckpoint(t.Context(), request)
	if err == nil || machine.closeCount != 0 || len(machine.snapshotRequests) != 0 {
		t.Fatalf("err=%v closes=%d", err, machine.closeCount)
	}
	if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes != limits.total {
		t.Fatal("existing owner reservation changed")
	}
}

func TestCheckpointPermanentUploadFailureRetainsSourceUntilSettlement(t *testing.T) {
	for _, media := range []string{cas.CheckpointVMConfigMediaType, cas.CheckpointVMStateMediaType, cas.CheckpointScratchDiskMediaType, cas.CheckpointMemoryMediaType} {
		t.Run(media, func(t *testing.T) {
			c, request, machine, store := newCaptureTest(t)
			failure := errors.New("permanent upload failure")
			store.publish = func(d cas.Descriptor, _ *os.File) error {
				if d.MediaType == media {
					return failure
				}
				return nil
			}
			_, err := c.CreateCheckpoint(t.Context(), request)
			if !errors.Is(err, failure) || machine.closeCount != 0 || machine.resumeCount != 0 {
				t.Fatalf("err=%v closes=%d resumes=%d", err, machine.closeCount, machine.resumeCount)
			}
			if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
				t.Fatal("failed capture released its staging before settlement")
			}
			if err := c.capture.ResumeGuestControl(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := c.capture.CompleteAbort(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := c.cleanupCheckpointStaging(); err != nil {
				t.Fatal(err)
			}
			if c.reservations.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
				t.Fatal("staging capacity leaked")
			}
			if !machine.artifact.Computer.Capture.(*versionCaptureFixture).released {
				t.Fatal("capture retention leaked")
			}
			assertRemoved(t, machine.artifact.VMState.Path)
			for _, file := range machine.artifact.Memory {
				assertRemoved(t, file.Path)
			}
			entries, err := os.ReadDir(c.tempDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging survived: %v %v", entries, err)
			}
		})
	}
}
