//go:build linux

package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type terminalCaptureSession struct {
	*checkpointSession
	pauseErr  error
	limitsErr error
	pauses    int
}

func (s *terminalCaptureSession) PauseComputer(context.Context) (*vm.RuntimeComputer, error) {
	s.pauses++
	return s.artifact.Computer, s.pauseErr
}
func (s *terminalCaptureSession) SnapshotLimits() (vm.SnapshotLimits, error) {
	if s.limitsErr != nil {
		return vm.SnapshotLimits{}, s.limitsErr
	}
	return s.checkpointSession.SnapshotLimits()
}
func newTerminalCaptureTest(t *testing.T) (terminalComputerCapturer, workerapi.RunLeaseAssignment, *terminalCaptureSession, *captureStore) {
	t.Helper()
	session := &terminalCaptureSession{checkpointSession: &checkpointSession{stream: newCheckpointStream(t, nil, "wait", "checkpoint"), artifact: checkpointArtifact(t)}}
	store := &captureStore{}
	c := terminalComputerCapturer{session: session, objects: store, capacity: testCheckpointCapacity(t), encryptor: testCheckpointEncryptor(t), tempDir: t.TempDir()}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	lease.WorkspaceID = session.artifact.Computer.ComputerID
	return c, lease, session, store
}

func TestTerminalComputerCaptureRegistersAndRetriesOneDisk(t *testing.T) {
	c, lease, session, store := newTerminalCaptureTest(t)
	source := bytes.Repeat([]byte{0x5a}, 4096)
	copy(source, []byte("post-task filesystem bytes"))
	if err := os.WriteFile(session.artifact.Computer.Path, source, 0600); err != nil {
		t.Fatal(err)
	}
	var registered workerapi.RegisterRunFinalizationRequest
	var registrations, uploads int
	var ciphertext []byte
	register := func(_ context.Context, req workerapi.RegisterRunFinalizationRequest) error {
		registrations++
		if uploads != 0 {
			t.Fatal("upload before registration completed")
		}
		if registrations == 1 {
			registered = req
			return retryableCaptureError{}
		}
		if registered != req {
			t.Fatal("registration retry changed candidate")
		}
		return nil
	}
	store.publish = func(d cas.Descriptor, file *os.File) error {
		uploads++
		if registrations != 2 || registered.Disk.Artifact.Digest != d.Digest {
			t.Fatal("unregistered upload")
		}
		if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
			t.Fatal("unreserved capture")
		}
		body, err := io.ReadAll(io.NewSectionReader(file, 0, d.SizeBytes))
		if err != nil {
			return err
		}
		if uploads == 1 {
			ciphertext = body
			return retryableCaptureError{}
		}
		if !bytes.Equal(ciphertext, body) {
			t.Fatal("upload retry reencrypted disk")
		}
		return nil
	}
	disk, err := c.capture(t.Context(), lease, "operation", register)
	if err != nil {
		t.Fatal(err)
	}
	if disk != registered.Disk || uploads != 2 || len(store.puts) != 1 || session.pauses != 1 || session.closeCount != 1 || len(session.snapshotRequests) != 0 {
		t.Fatalf("capture=%+v uploads=%d pauses=%d closes=%d snapshots=%d", disk, uploads, session.pauses, session.closeCount, len(session.snapshotRequests))
	}
	if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
		t.Fatal("reservation leaked")
	}
	target := filepath.Join(t.TempDir(), "restored.disk")
	artifact := computer.DiskArtifact{Object: cas.Descriptor{Digest: disk.Artifact.Digest, SizeBytes: disk.Artifact.SizeBytes, MediaType: disk.Artifact.MediaType}, LogicalBytes: disk.LogicalBytes}
	if err := (computer.DiskStore{CAS: store, Cipher: c.encryptor}).Restore(t.Context(), disk.ComputerID, artifact, target, disk.LogicalBytes); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(source, restored) {
		t.Fatalf("terminal disk roundtrip failed: %v", err)
	}
	entries, err := os.ReadDir(c.tempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging remains: %v %v", entries, err)
	}
}

func TestTerminalComputerCaptureFailureAlwaysStopsSource(t *testing.T) {
	for _, stage := range []string{"dependencies", "limits", "capacity", "pause", "identity", "registration", "upload", "stop"} {
		t.Run(stage, func(t *testing.T) {
			c, lease, session, store := newTerminalCaptureTest(t)
			failure := errors.New("injected capture failure")
			register := func(context.Context, workerapi.RegisterRunFinalizationRequest) error { return nil }
			switch stage {
			case "dependencies":
				c.encryptor = nil
			case "limits":
				session.limitsErr = failure
			case "capacity":
				c.capacity, _ = capacity.New(capacity.Vector{CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: 1})
			case "pause":
				session.pauseErr = failure
			case "identity":
				lease.WorkspaceID = "different"
			case "registration":
				register = func(context.Context, workerapi.RegisterRunFinalizationRequest) error {
					return &httpclient.Error{StatusCode: 409, Status: "409 Conflict"}
				}
			case "upload":
				store.publish = func(cas.Descriptor, *os.File) error { return failure }
			case "stop":
				session.closeErr = failure
			}
			_, err := c.capture(t.Context(), lease, "operation", register)
			if err == nil || session.closeCount != 1 || len(session.snapshotRequests) != 0 {
				t.Fatalf("error=%v close=%d snapshots=%d", err, session.closeCount, len(session.snapshotRequests))
			}
			if stage == "stop" {
				if !errors.Is(err, failure) {
					t.Fatal("unproved stop lost cause")
				}
			}
			if c.capacity.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
				t.Fatal("reservation leaked after proved stop")
			}
		})
	}
}
