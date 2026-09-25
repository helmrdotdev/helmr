//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type liveCaptureDevice struct {
	ownedComputerFixture
	capture func(context.Context) (computer.CapturedGeneration, error)
}

func (d *liveCaptureDevice) Capture(ctx context.Context) (computer.CapturedGeneration, error) {
	if d.capture != nil {
		return d.capture(ctx)
	}
	return d.ownedComputerFixture.Capture(ctx)
}

// Exercises the actual SDK HTTP boundary, not a running VMM.
func liveCaptureSession(t *testing.T, resumeFailure bool) (*guestSession, *liveCaptureDevice, func() []string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"computer.ext4", "scratch.ext4"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("disk"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var states []string
	socket := serveSnapshotAPI(t, root, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/vm/config" {
			_, _ = w.Write([]byte(`{"drives":[{"drive_id":"computer","io_engine":"Sync","cache_type":"Writeback","is_read_only":false,"path_on_host":"/computer.ext4"},{"drive_id":"scratch","io_engine":"Sync","cache_type":"Writeback","is_read_only":false,"path_on_host":"/scratch.ext4"}]}`))
			return
		}
		if r.Method != http.MethodPatch || r.URL.Path != "/vm" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var body struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		states = append(states, body.State)
		mu.Unlock()
		if resumeFailure && body.State == "Resumed" {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	})
	machine, err := sdk.NewMachine(t.Context(), sdk.Config{SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	files, err := openRuntimeDiskFiles(filepath.Join(root, "scratch.ext4"), filepath.Join(root, "computer.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeRuntimeDiskFiles(files) })
	device := &liveCaptureDevice{}
	session := &guestSession{machine: machine, diskFiles: files, jailRoot: root, scratchDisk: filepath.Join(root, "scratch.ext4"), topology: vm.RuntimeTopology{Computer: &vm.RuntimeComputer{ComputerID: "computer", Path: filepath.Join(root, "computer.ext4"), Device: device}}}
	return session, device, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), states...) }
}

func TestLiveComputerCaptureResumesBeforePublication(t *testing.T) {
	s, d, states := liveCaptureSession(t, false)
	cut, err := s.CaptureComputer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := states(); len(got) != 2 || got[0] != "Paused" || got[1] != "Resumed" {
		t.Fatalf("states %v", got)
	}
	if d.releases != 0 {
		t.Fatal("released caller's capture")
	}
	cut.Capture.Release()
	terminal, err := s.PauseComputer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	terminal.Capture.Release()
	if _, err := s.CaptureComputer(t.Context()); err == nil {
		t.Fatal("resumed terminal hold")
	}
	if got := states(); len(got) != 3 || got[2] != "Paused" {
		t.Fatalf("states %v", got)
	}
}

func TestLiveComputerCaptureResumeFailureRetainsHold(t *testing.T) {
	s, d, _ := liveCaptureSession(t, true)
	if _, err := s.CaptureComputer(t.Context()); err == nil {
		t.Fatal("accepted failed resume")
	}
	if d.captures != 1 || d.releases != 1 {
		t.Fatal("unreturned capture leaked")
	}
	if _, err := s.CaptureComputer(t.Context()); err == nil {
		t.Fatal("reused ambiguous source")
	}
}

func TestLiveComputerCaptureCheckpointWaitsForResume(t *testing.T) {
	s, d, states := liveCaptureSession(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	d.capture = func(ctx context.Context) (computer.CapturedGeneration, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &ownedCaptureFixture{owner: &d.ownedComputerFixture}, nil
	}
	result := make(chan error, 1)
	go func() {
		cut, err := s.CaptureComputer(t.Context())
		if cut != nil {
			cut.Capture.Release()
		}
		result <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.PauseComputer(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	d.capture = nil
	cut, err := s.PauseComputer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cut.Capture.Release()
	got := states()
	if len(got) != 3 || got[0] != "Paused" || got[1] != "Resumed" || got[2] != "Paused" {
		t.Fatalf("states %v", got)
	}
}

func TestCloseCancelsCaptureBeforeReleasingBarrier(t *testing.T) {
	s, d, states := liveCaptureSession(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	d.capture = func(ctx context.Context) (computer.CapturedGeneration, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}
	result := make(chan error, 1)
	go func() { _, err := s.CaptureComputer(t.Context()); result <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close wait: %v", err)
	}
	if _, err := s.diskFiles["computer"].Stat(); err != nil {
		t.Fatal("closed disk before capture joined")
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("capture: %v", err)
	}
	if got := states(); len(got) != 1 || got[0] != "Paused" {
		t.Fatalf("resumed closing source: %v", got)
	}
	// A timed-out close has not consumed the once-only cleanup operation.
	ran := false
	s.once.Do(func() { ran = true })
	if !ran {
		t.Fatal("timed-out close consumed cleanup")
	}
}

func TestLiveComputerCaptureFailureNeverResumes(t *testing.T) {
	for _, stage := range []string{"backing sync", "device capture"} {
		t.Run(stage, func(t *testing.T) {
			s, d, states := liveCaptureSession(t, false)
			if stage == "backing sync" {
				if err := os.Remove(s.scratchDisk); err != nil {
					t.Fatal(err)
				}
			} else {
				d.capture = func(context.Context) (computer.CapturedGeneration, error) {
					return nil, errors.New("device capture failed")
				}
			}
			if _, err := s.CaptureComputer(t.Context()); err == nil {
				t.Fatal("accepted failed capture")
			}
			if _, err := s.CaptureComputer(t.Context()); err == nil {
				t.Fatal("reused failed source")
			}
			if got := states(); len(got) != 1 || got[0] != "Paused" {
				t.Fatalf("states %v", got)
			}
		})
	}
}

func TestLiveComputerCaptureSerializesTerminalCut(t *testing.T) {
	s, d, states := liveCaptureSession(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	d.capture = func(ctx context.Context) (computer.CapturedGeneration, error) {
		first.Do(func() { close(entered); <-release })
		return &ownedCaptureFixture{owner: &d.ownedComputerFixture}, nil
	}
	// Retain both cuts until both operations join; their fake owner is not concurrent.
	live, terminal := make(chan *vm.ComputerSnapshot, 1), make(chan *vm.ComputerSnapshot, 1)
	errs := make(chan error, 2)
	go func() { cut, err := s.CaptureComputer(t.Context()); live <- cut; errs <- err }()
	<-entered
	go func() { cut, err := s.PauseComputer(t.Context()); terminal <- cut; errs <- err }()
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	(<-live).Capture.Release()
	(<-terminal).Capture.Release()
	if _, err := s.CaptureComputer(t.Context()); err == nil {
		t.Fatal("resumed terminal cut")
	}
	if got := states(); len(got) != 3 || got[0] != "Paused" || got[1] != "Resumed" || got[2] != "Paused" {
		t.Fatalf("states %v", got)
	}
}
