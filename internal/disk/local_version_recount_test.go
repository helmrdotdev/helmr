//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/cas"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type pausedRootStore struct {
	captureObjectBatch
	entered, release chan struct{}
}

func (g *pausedRootStore) StoreObject(ctx context.Context, d [32]byte, raw []byte) error {
	if len(raw) > 8 && string(raw[:4]) == "HGP0" && bytes.Contains(raw[8:min(len(raw), 50)], []byte(`"Rank":3,`)) {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.captureObjectBatch.StoreObject(ctx, d, raw)
}
func TestLocalCollectionCensusPreservesInflightCharges(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), []byte{9}, 7); err != nil {
		t.Fatal(err)
	}
	sink := p.disk.writer.Sink.(*reservedVersionSink)
	gate := &pausedRootStore{captureObjectBatch: p.objectBatch(), entered: make(chan struct{}), release: make(chan struct{})}
	p.objectBatch = func() captureObjectBatch { return gate }
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	captured := make(chan error, 1)
	go func() {
		c, e := p.Capture(t.Context())
		if c != nil {
			c.Release()
		}
		captured <- e
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("encoder did not reach final root store")
	}
	// A normal-build wait hook fires immediately before Collect takes commit,
	// after its complete census. This gives exact ordering without sleeps.
	scanned := make(chan struct{})
	var once sync.Once
	ctx := cas.WithGuestIOWait(t.Context(), func(waiting bool) {
		if waiting {
			once.Do(func() { close(scanned) })
		}
	})
	collected := make(chan error, 1)
	go func() { _, e := p.Collect(ctx, 1000); collected <- e }()
	select {
	case <-scanned:
	case e := <-collected:
		if e != nil {
			t.Fatal(e)
		}
		collected <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not finish or yield census")
	}
	release.Do(func() { close(gate.release) })
	if e := <-captured; e != nil {
		t.Fatal(e)
	}
	if e := <-collected; e != nil {
		t.Fatal(e)
	}
	var actual int64
	entries, e := os.ReadDir(filepath.Join(cfg.Directory, "objects", "sha256"))
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		info, e := entry.Info()
		if e != nil {
			t.Fatal(e)
		}
		actual += info.Size()
	}
	sink.mu.Lock()
	remaining := sink.remaining
	sink.mu.Unlock()
	if remaining != cfg.StagedBytes-actual {
		t.Fatalf("staging over-credit: remaining=%d expected=%d uncharged=%d actual=%d", remaining, cfg.StagedBytes-actual, remaining-(cfg.StagedBytes-actual), actual)
	}
}

func TestLocalCollectionRejectsCensusOverlappedByCapture(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	scanned, release := make(chan struct{}), make(chan struct{})
	var once, finish sync.Once
	defer finish.Do(func() { close(release) })
	ctx := cas.WithGuestIOWait(t.Context(), func(waiting bool) {
		if waiting {
			once.Do(func() { close(scanned); <-release })
		}
	})
	collected := make(chan error, 1)
	go func() {
		n, e := p.Collect(ctx, 1000)
		if n != 0 && e == nil {
			e = fmt.Errorf("stale census removed %d bytes", n)
		}
		collected <- e
	}()
	select {
	case <-scanned:
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not finish census")
	}
	if _, err = p.WriteAt(t.Context(), []byte{9}, 7); err != nil {
		t.Fatal(err)
	}
	c, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	sink := p.disk.writer.Sink.(*reservedVersionSink)
	sink.mu.Lock()
	before := sink.remaining
	sink.mu.Unlock()
	finish.Do(func() { close(release) })
	if err = <-collected; err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	after := sink.remaining
	sink.mu.Unlock()
	if after != before {
		t.Fatalf("stale census changed available bytes: %d -> %d", before, after)
	}
}

func TestLocalCollectionReportsStableMalformedEntry(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	dir := filepath.Join(cfg.Directory, "objects", "sha256")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ".object-incomplete"), []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Collect(t.Context(), 1000); err == nil {
		t.Fatal("stable malformed entry was ignored")
	}
}
