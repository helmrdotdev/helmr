//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type captureResult struct {
	cut *LocalCapture
	err error
}

type blockingCaptureSink struct {
	target  blockformat.ObjectSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

func (s *blockingCaptureSink) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	s.once.Do(func() {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	})
	if s.err != nil {
		return s.err
	}
	return s.target.StoreObject(ctx, digest, raw)
}

func TestOnlineCaptureAllowsRequestsDuringEncoding(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "published-cut", true: "failed-encoding"}[fail], func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			device := versionDevice{p}
			if _, err = device.WriteAt(t.Context(), []byte{2}, 7); err != nil {
				t.Fatal(err)
			}
			reserved := p.disk.writer.Sink.(*reservedVersionSink)
			sink := &blockingCaptureSink{target: reserved.target, entered: make(chan struct{}), release: make(chan struct{})}
			if fail {
				sink.err = errors.New("capture encoding failed")
			}
			reserved.target = sink
			done := make(chan captureResult, 1)
			go func() { cut, err := p.Capture(t.Context()); done <- captureResult{cut, err} }()
			<-sink.entered
			// Encoding is blocked. A complete device request must still finish,
			// and overwriting a frozen block must not alter the captured version.
			if _, err = device.WriteAt(t.Context(), []byte{3}, 7); err != nil {
				t.Fatal(err)
			}
			if err = device.Trim(t.Context(), 4096, 4096); err != nil {
				t.Fatal(err)
			}
			if got := localByte(t, p); got != 3 {
				t.Fatalf("live byte = %d", got)
			}
			close(sink.release)
			result := <-done
			if fail {
				if !errors.Is(result.err, sink.err) || result.cut != nil {
					t.Fatalf("failed capture: %+v", result)
				}
			} else {
				if result.err != nil {
					t.Fatal(result.err)
				}
				defer result.cut.Release()
				tree, err := OpenVersion(t.Context(), p.disk.writer.Source, cfg.Scope, cfg.Keys, result.cut.Root(), cfg.Base.LogicalBytes)
				if err != nil {
					t.Fatal(err)
				}
				b, err := tree.ReadBlock(t.Context(), 0)
				if err != nil || b[7] != 2 {
					t.Fatalf("saved cut changed: %v %v", b, err)
				}
			}
			if got := localByte(t, p); got != 3 {
				t.Fatalf("successor lost: %d", got)
			}
		})
	}
}

func TestOnlineCaptureDoesNotSplitPressureFlushedRequest(t *testing.T) {
	for _, trim := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "trim"}[trim], func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			cfg.DirtyBlocks = 1
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if trim {
				seed := bytes.Repeat([]byte{9}, 4*4096)
				if _, err := (versionDevice{p}).WriteAt(t.Context(), seed, 0); err != nil {
					t.Fatal(err)
				}
				if _, err := p.Flush(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			p.phase = func(phase string) error {
				if phase == "objects-staged" {
					once.Do(func() { close(entered); <-release })
				}
				return nil
			}
			body := bytes.Repeat([]byte{9}, 3*4096)
			requestDone := make(chan error, 1)
			go func() {
				device := versionDevice{p}
				var err error
				if trim {
					err = device.Trim(t.Context(), 3, len(body))
				} else {
					_, err = device.WriteAt(t.Context(), body, 3)
				}
				requestDone <- err
			}()
			<-entered // First pressure flush, with most of the request still unapplied.
			if p.request.TryLock() {
				p.request.Unlock()
				t.Fatal("request boundary was released mid-request")
			}
			done := make(chan captureResult, 1)
			go func() { cut, err := p.Capture(t.Context()); done <- captureResult{cut, err} }()
			close(release)
			if err = <-requestDone; err != nil {
				t.Fatal(err)
			}
			result := <-done
			if result.err != nil {
				t.Fatal(result.err)
			}
			defer result.cut.Release()
			tree, err := OpenVersion(t.Context(), p.disk.writer.Source, cfg.Scope, cfg.Keys, result.cut.Root(), cfg.Base.LogicalBytes)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tree.ReadRange(t.Context(), 3, len(body))
			if trim {
				clear(body)
			}
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("cut split a complete request: %v", err)
			}
		})
	}
}

func TestOnlineCaptureReleasesRequestGateWithoutDirtyData(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cut, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cut.Release()
	if !p.request.TryLock() {
		t.Fatal("clean capture stranded the request gate")
	}
	p.request.Unlock()
}

func TestOnlineCaptureWaitingForCommitDoesNotHoldRequests(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := (versionDevice{p}).WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.phase = func(phase string) error {
		if phase == "root-synced" {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	first := make(chan captureResult, 1)
	go func() { cut, err := p.Capture(t.Context()); first <- captureResult{cut, err} }()
	<-entered
	second := make(chan captureResult, 1)
	started := make(chan struct{})
	go func() { close(started); cut, err := p.Capture(t.Context()); second <- captureResult{cut, err} }()
	<-started
	// First cut is blocked during root persistence; a second capture is scheduled.
	// The successor request must complete before either capture can finish.
	if _, err := (versionDevice{p}).WriteAt(t.Context(), []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	close(release)
	for _, result := range []captureResult{<-first, <-second} {
		if result.err != nil {
			t.Fatal(result.err)
		}
		result.cut.Release()
	}
}
