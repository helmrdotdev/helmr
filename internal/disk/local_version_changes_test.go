//go:build linux || darwin

package disk

import (
	"context"
	"errors"
	"testing"
)

func TestLocalVersionChangesCaptureBoundary(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	initial := p.Changes()
	if initial.Generation != 0 || !initial.ChangedAt.IsZero() {
		t.Fatal("new writer is dirty")
	}
	if _, err := p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	first := p.Changes()
	select {
	case <-initial.Wake:
	default:
		t.Fatal("mutation did not wake observer")
	}
	newBatch := p.objectBatch
	sink := &blockingCaptureSink{target: newBatch(), entered: make(chan struct{}), release: make(chan struct{})}
	p.objectBatch = func() captureObjectBatch { return sink }
	done := make(chan captureResult, 1)
	go func() { cut, err := p.Capture(t.Context()); done <- captureResult{cut, err} }()
	<-sink.entered
	if _, err := p.WriteAt(t.Context(), []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	successor := p.Changes()
	close(sink.release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.cut.Release()
	if result.cut.Changes().Generation != first.Generation || successor.Generation != first.Generation+1 || successor.ChangedAt.Before(first.ChangedAt) {
		t.Fatalf("cut=%d first=%+v successor=%+v", result.cut.Changes().Generation, first, successor)
	}
	next, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if next.Changes().Generation != successor.Generation || !p.Changes().ChangedAt.IsZero() {
		t.Fatal("capture lost dirty boundary")
	}
}

func TestLocalVersionCaptureRetryAfterPersistenceAmbiguity(t *testing.T) {
	for _, phase := range []string{"root-renamed", "root-committed"} {
		t.Run(phase, func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if _, err := p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
				t.Fatal(err)
			}
			p.phase = func(at string) error {
				if at == phase {
					return errors.New("lost persistence receipt")
				}
				return nil
			}
			if cut, err := p.Capture(t.Context()); err == nil || cut != nil {
				t.Fatal("failed receipt became a capture")
			}
			p.phase = nil
			if _, err := p.WriteAt(t.Context(), []byte{3}, 8); err != nil {
				t.Fatal(err)
			}
			cut, err := p.Capture(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer cut.Release()
			tree, err := OpenVersion(t.Context(), p.disk.writer.Source, cfg.Scope, cfg.Keys, cut.Root(), cfg.Base.LogicalBytes)
			if err != nil {
				t.Fatal(err)
			}
			block, err := tree.ReadBlock(t.Context(), 0)
			if err != nil || block[7] != 2 || block[8] != 3 || cut.Changes().Generation != 2 {
				t.Fatalf("retry lost acknowledged prefix: %v", err)
			}
		})
	}
}

func TestLocalVersionCanceledCaptureKeepsDirtyGeneration(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	before := p.Changes()
	newBatch := p.objectBatch
	sink := &blockingCaptureSink{target: newBatch(), entered: make(chan struct{}), release: make(chan struct{})}
	p.objectBatch = func() captureObjectBatch { return sink }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan captureResult, 1)
	go func() { cut, err := p.Capture(ctx); done <- captureResult{cut, err} }()
	<-sink.entered
	cancel()
	result := <-done
	if !errors.Is(result.err, context.Canceled) || result.cut != nil {
		t.Fatalf("capture=%+v", result)
	}
	after := p.Changes()
	if after.Generation != before.Generation || !after.ChangedAt.Equal(before.ChangedAt) {
		t.Fatal("cancellation lost dirty age")
	}
	p.objectBatch = newBatch
	cut, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cut.Release()
	if cut.Changes().Generation != before.Generation || localByte(t, p) != 2 {
		t.Fatal("cancellation lost dirty data")
	}
}

// An fsync or a full write buffer stages objects locally but does not publish a
// recovery head. Its boundary must not erase the autosave scheduler's deadline.
func TestLocalVersionAutosaveAgeSurvivesGuestFlush(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	first := p.Changes()
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	afterFlush := p.Changes()
	if afterFlush.Generation != first.Generation || !afterFlush.ChangedAt.Equal(first.ChangedAt) || !afterFlush.LastChangedAt.Equal(first.LastChangedAt) {
		t.Fatalf("guest flush erased pending interval: before=%+v after=%+v", first, afterFlush)
	}
	if err = p.Trim(t.Context(), 8, 1); err != nil {
		t.Fatal(err)
	}
	second := p.Changes()
	if !second.ChangedAt.Equal(first.ChangedAt) || second.LastChangedAt.Before(first.LastChangedAt) || second.Generation != first.Generation+1 {
		t.Fatalf("trim moved the first deadline or lost the latest mutation: %+v", second)
	}
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	cut, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cut.Release()
	captured := cut.Changes()
	if captured.Generation != second.Generation || !captured.ChangedAt.Equal(first.ChangedAt) || !captured.LastChangedAt.Equal(second.LastChangedAt) {
		t.Fatalf("clean owner cut lost staged mutation interval: %+v", captured)
	}
	if changes := p.Changes(); !changes.ChangedAt.IsZero() || !changes.LastChangedAt.IsZero() {
		t.Fatalf("owner cut did not transfer the pending interval: %+v", changes)
	}
}

func TestLocalVersionFailedCaptureMergesConcurrentMutationAge(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	first := p.Changes()
	newBatch := p.objectBatch
	sink := &blockingCaptureSink{target: newBatch(), entered: make(chan struct{}), release: make(chan struct{})}
	p.objectBatch = func() captureObjectBatch { return sink }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan captureResult, 1)
	go func() { cut, err := p.Capture(ctx); done <- captureResult{cut, err} }()
	<-sink.entered
	if _, err = p.WriteAt(t.Context(), []byte{3}, 8); err != nil {
		t.Fatal(err)
	}
	later := p.Changes()
	cancel()
	if result := <-done; !errors.Is(result.err, context.Canceled) || result.cut != nil {
		t.Fatalf("capture=%+v", result)
	}
	merged := p.Changes()
	if merged.Generation != later.Generation || !merged.ChangedAt.Equal(first.ChangedAt) || !merged.LastChangedAt.Equal(later.LastChangedAt) {
		t.Fatalf("failed capture replaced concurrent interval: first=%+v later=%+v merged=%+v", first, later, merged)
	}
}
