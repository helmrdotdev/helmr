package blockformat_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

type writerSink struct {
	store         *cas.File
	calls, failAt int
	cancel        context.CancelFunc
	objects       map[[32]byte]bool
}

func (s *writerSink) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	s.calls++
	if s.failAt == s.calls {
		return errors.New("injected stage failure")
	}
	if err := s.store.StoreObject(ctx, digest, raw); err != nil {
		return err
	}
	s.objects[digest] = true
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}
func writerFixture(t *testing.T) (blockformat.Writer, *writerSink) {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &writerSink{store: store, objects: map[[32]byte]bool{}}
	return blockformat.Writer{Source: store, Sink: sink, Scope: "scope", ActiveKey: "first", Keys: map[string][]byte{"first": bytes.Repeat([]byte{1}, 32), "second": bytes.Repeat([]byte{2}, 32)}, PackLimit: blockformat.MinPackLimit}, sink
}
func readWritten(t *testing.T, w blockformat.Writer, root blockformat.Locator, block uint64, want byte) {
	t.Helper()
	tree, err := blockformat.OpenTree(t.Context(), w.Source, w.Scope, w.Keys, root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tree.ReadBlock(t.Context(), block)
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{want}, 4096)) {
		t.Fatalf("read block %d: %v", block, err)
	}
}
func TestCaptureGenerations(t *testing.T) {
	for _, fanout := range []int{64, 256} {
		t.Run(fmt.Sprint(fanout), func(t *testing.T) {
			w, sink := writerFixture(t)
			const capacity = 32 << 30
			empty, err := w.Empty(t.Context(), capacity, fanout)
			if err != nil {
				t.Fatal(err)
			}
			first, err := w.Capture(t.Context(), empty, capacity, map[uint64][]byte{0: bytes.Repeat([]byte{7}, 4096), 8192: bytes.Repeat([]byte{8}, 4096)})
			if err != nil {
				t.Fatal(err)
			}
			w.ActiveKey = "second"
			second, err := w.Capture(t.Context(), first, capacity, map[uint64][]byte{0: make([]byte, 4096), 16384: bytes.Repeat([]byte{9}, 4096)})
			if err != nil {
				t.Fatal(err)
			}
			readWritten(t, w, empty, 0, 0)
			readWritten(t, w, first, 0, 7)
			readWritten(t, w, first, 8192, 8)
			readWritten(t, w, second, 0, 0)
			readWritten(t, w, second, 8192, 8)
			readWritten(t, w, second, 16384, 9)
			count := sink.calls
			same, err := w.Capture(t.Context(), second, capacity, nil)
			if err != nil || same != second || sink.calls != count {
				t.Fatal("empty capture emitted bytes or changed identity")
			}
			branch, err := w.Capture(t.Context(), first, capacity, map[uint64][]byte{0: bytes.Repeat([]byte{5}, 4096)})
			if err != nil {
				t.Fatal(err)
			}
			readWritten(t, w, branch, 0, 5)
			readWritten(t, w, first, 0, 7)
			readWritten(t, w, second, 0, 0)
			allZero, err := w.Capture(t.Context(), second, capacity, map[uint64][]byte{8192: make([]byte, 4096), 16384: make([]byte, 4096)})
			if err != nil {
				t.Fatal(err)
			}
			readWritten(t, w, allZero, 8192, 0)
			readWritten(t, w, allZero, 16384, 0)
		})
	}
}
func TestCaptureFailuresPreserveBase(t *testing.T) {
	w, sink := writerFixture(t)
	const capacity = 32 << 30
	base, err := w.Empty(t.Context(), capacity, 64)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[uint64][]byte{0: bytes.Repeat([]byte{7}, 4096), 8192: bytes.Repeat([]byte{8}, 4096)}
	before := sink.calls
	if _, err = w.Capture(t.Context(), base, capacity, changes); err != nil {
		t.Fatal(err)
	}
	writes := sink.calls - before
	for failure := 1; failure <= writes; failure++ {
		sink.failAt = sink.calls + failure
		got, err := w.Capture(t.Context(), base, capacity, changes)
		if err == nil || got != (blockformat.Locator{}) {
			t.Fatalf("failure %d returned usable root", failure)
		}
		readWritten(t, w, base, 0, 0)
	}
	sink.failAt = 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink.cancel = cancel
	if got, err := w.Capture(ctx, base, capacity, changes); err == nil || got != (blockformat.Locator{}) {
		t.Fatal("cancel returned usable root")
	}
	sink.cancel = nil
	readWritten(t, w, base, 0, 0)
	count := sink.calls
	for _, bad := range []map[uint64][]byte{{0: {1}}, {uint64(capacity / 4096): make([]byte, 4096)}} {
		if _, err = w.Capture(t.Context(), base, capacity, bad); err == nil || sink.calls != count {
			t.Fatal("invalid changes emitted bytes")
		}
	}
	if _, err = w.Capture(t.Context(), base, capacity*2, changes); err == nil || sink.calls != count {
		t.Fatal("wrong capacity emitted bytes")
	}
	tooMany := make(map[uint64][]byte, blockformat.MaxChangedBlocks+1)
	for i := 0; i <= blockformat.MaxChangedBlocks; i++ {
		tooMany[uint64(i)] = nil
	}
	if _, err = w.Capture(t.Context(), base, capacity, tooMany); err == nil || sink.calls != count {
		t.Fatal("unbounded changes emitted bytes")
	}
}

func TestConcurrentImmutableObjectStaging(t *testing.T) {
	_, sink := writerFixture(t)
	raw := []byte("immutable")
	// Exercise only the concurrent concrete store, not the single-owner fixture sink.
	digest := sha256.Sum256(raw)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- sink.store.StoreObject(t.Context(), digest, raw) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.store.StoreObject(t.Context(), digest, []byte("different")); err == nil {
		t.Fatal("mismatched bytes accepted")
	}
}

func TestCaptureDenseHistory(t *testing.T) {
	w, sink := writerFixture(t)
	w.PackLimit = blockformat.MinPackLimit - 1
	if _, err := w.Empty(t.Context(), 256<<20, 256); err == nil || sink.calls != 0 {
		t.Fatal("undersized pack admitted")
	}
	w.PackLimit = blockformat.MinPackLimit
	// JSON expands each key byte to six bytes. Every update creates a new segment.
	w.ActiveKey = string(bytes.Repeat([]byte{0}, 128))
	w.Keys[w.ActiveKey] = bytes.Repeat([]byte{3}, 32)
	const capacity = 1 << 30
	root, err := w.Empty(t.Context(), capacity, 256)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < 256; i++ {
		root, err = w.Capture(t.Context(), root, capacity, map[uint64][]byte{i: bytes.Repeat([]byte{1}, 4096), i*256 + 256: bytes.Repeat([]byte{2}, 4096)})
		if err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	for i := uint64(0); i < 256; i++ {
		readWritten(t, w, root, i, 1)
	}
}
