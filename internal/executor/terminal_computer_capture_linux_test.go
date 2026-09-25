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
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type terminalCaptureSession struct {
	*checkpointSession
	disk                            *computer.LocalGeneration
	pauseErr, limitsErr, publishErr error
	pauses                          int
	captures, releases              int
}

func (s *terminalCaptureSession) PauseComputer(ctx context.Context) (*vm.ComputerSnapshot, error) {
	s.pauses++
	if s.pauseErr != nil {
		return nil, s.pauseErr
	}
	capture, err := s.disk.Capture(ctx)
	if err != nil {
		return nil, err
	}
	s.captures++
	return &vm.ComputerSnapshot{ComputerID: s.artifact.Computer.ComputerID, Capture: &generationCaptureFixture{root: capture.Root(), publish: func(ctx context.Context, p computer.ContinuationPublication) error {
		if s.publishErr != nil {
			return s.publishErr
		}
		return capture.Publish(ctx, p)
	}, release: func() { s.releases++; capture.Release() }}}, nil
}
func (s *terminalCaptureSession) SnapshotLimits() (vm.SnapshotLimits, error) {
	if s.limitsErr != nil {
		return vm.SnapshotLimits{}, s.limitsErr
	}
	return s.checkpointSession.SnapshotLimits()
}
func (s *terminalCaptureSession) Close(ctx context.Context) error {
	err := s.checkpointSession.Close(ctx)
	if err != nil {
		return err
	}
	return s.disk.Close()
}

type terminalPublicationFixture struct {
	*cas.File
	registered bool
	uploads    int
	retry      bool
}

func (p *terminalPublicationFixture) Register(context.Context, blockformat.ObjectInspection) error {
	if !p.registered {
		return errors.New("object before candidate registration")
	}
	return nil
}
func (p *terminalPublicationFixture) Certify(context.Context, blockformat.ObjectInspection) error {
	return nil
}
func (p *terminalPublicationFixture) Reuse(context.Context, blockformat.ObjectInspection) error {
	return nil
}
func (p *terminalPublicationFixture) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (o cas.Object, err error) {
	return runComputerPublisher{objects: p}.Upload(ctx, d, f)
}
func (p *terminalPublicationFixture) Publish(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	p.uploads++
	o, err := p.File.Put(ctx, d.MediaType, io.NewSectionReader(f, 0, d.SizeBytes))
	if err == nil && p.retry {
		p.retry = false
		return o, retryableCaptureError{}
	}
	return o, err
}
func newTerminalCaptureTest(t *testing.T) (terminalComputerCapturer, workerapi.RunLeaseAssignment, *terminalCaptureSession, *terminalPublicationFixture, computer.LocalGenerationConfig) {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyID := uuid.NewV7().String()
	keys := map[string][]byte{keyID: bytes.Repeat([]byte{7}, 32)}
	writer := blockformat.Writer{Source: store, Sink: store, Scope: "terminal-fixture", ActiveKey: keyID, Keys: keys, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 4096, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, 4096)
	if err != nil {
		t.Fatal(err)
	}
	cfg := computer.LocalGenerationConfig{Directory: filepath.Join(t.TempDir(), "generation"), Base: root, BaseSource: store, Scope: writer.Scope, ActiveKey: keyID, Keys: keys, DirtyBlocks: 2, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit}
	disk, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	session := &terminalCaptureSession{disk: disk, checkpointSession: &checkpointSession{stream: newCheckpointStream(t, nil, "wait", "checkpoint"), artifact: checkpointArtifact(t)}}
	publisher := &terminalPublicationFixture{File: store}
	c := terminalComputerCapturer{session: session, publication: func(workerapi.RunLeaseAssignment, string) computer.ContinuationPublication { return publisher }}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	lease.WorkspaceID = session.artifact.Computer.ComputerID
	return c, lease, session, publisher, cfg
}
func TestTerminalComputerCaptureRegistersAndRestoresGeneration(t *testing.T) {
	c, lease, session, p, cfg := newTerminalCaptureTest(t)
	data := bytes.Repeat([]byte{42}, 4096)
	if _, err := session.disk.WriteAt(t.Context(), data, 0); err != nil {
		t.Fatal(err)
	}
	var candidate workerapi.RegisterRunFinalizationRequest
	registrations := 0
	p.retry = true
	got, err := c.capture(t.Context(), lease, "operation", func(_ context.Context, r workerapi.RegisterRunFinalizationRequest) error {
		registrations++
		if registrations == 1 {
			candidate = r
			return retryableCaptureError{}
		}
		if candidate != r {
			t.Fatal("retry changed frozen candidate")
		}
		p.registered = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := session; s.captures != 1 || s.releases != 1 {
		t.Fatalf("capture ownership: %d/%d", s.captures, s.releases)
	}
	if got != candidate.Disk || session.pauses != 1 || session.closeCount != 1 || registrations != 2 || p.uploads < 2 {
		t.Fatalf("capture/stop/retry mismatch: %+v", got)
	}
	// Reopen from only remotely published bytes after the source Runtime ended.
	cfg.Directory = filepath.Join(t.TempDir(), "restored")
	cfg.Base = got.Root
	restored, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	read := make([]byte, len(data))
	if _, err = restored.ReadAt(t.Context(), read, 0); err != nil || !bytes.Equal(data, read) {
		t.Fatalf("restored bytes: %v", err)
	}
}
func TestTerminalComputerCaptureFailureAlwaysStopsSource(t *testing.T) {
	for _, stage := range []string{"dependencies", "limits", "pause", "identity", "registration", "upload", "stop"} {
		t.Run(stage, func(t *testing.T) {
			c, lease, s, p, _ := newTerminalCaptureTest(t)
			p.registered = true
			failure := errors.New("injected capture failure")
			register := func(context.Context, workerapi.RegisterRunFinalizationRequest) error { return nil }
			switch stage {
			case "dependencies":
				c.publication = nil
			case "limits":
				s.limitsErr = failure
			case "pause":
				s.pauseErr = failure
			case "identity":
				lease.WorkspaceID = "other"
			case "registration":
				register = func(context.Context, workerapi.RegisterRunFinalizationRequest) error {
					return &httpclient.Error{StatusCode: 409, Status: "409 Conflict"}
				}
			case "upload":
				s.publishErr = failure
			case "stop":
				s.closeErr = failure
			}
			_, err := c.capture(t.Context(), lease, "operation", register)
			if s.releases != s.captures {
				t.Fatalf("capture retention leaked: captures=%d releases=%d", s.captures, s.releases)
			}
			if err == nil || s.closeCount != 1 {
				t.Fatalf("error=%v stops=%d", err, s.closeCount)
			}
			if stage == "stop" {
				var release *checkpointSourceReleaseError
				if !errors.As(err, &release) || !errors.Is(err, failure) {
					t.Fatal("stop proof failure lost")
				}
			}
		})
	}
}
