//go:build linux || darwin

package computer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func TestLocalCollectionReusesBudgetAcrossOverwritesAndReopen(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	cfg.StagedBytes = 128 << 10
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { p.Close() }()
	var reclaimed int64
	for i := 1; i <= 80; i++ {
		if _, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{byte(i)}, 4096), 0); err != nil {
			t.Fatal(err)
		}
		if _, err = p.Flush(t.Context()); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
		n, err := p.Collect(t.Context(), 1000)
		if err != nil {
			t.Fatalf("collect %d: %v", i, err)
		}
		reclaimed += n
		if localByte(t, p) != byte(i) {
			t.Fatal("collection lost current data")
		}
		if i == 40 {
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
			p, err = OpenLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if reclaimed <= cfg.StagedBytes {
		t.Fatalf("did not exceed staging over lifetime: %d", reclaimed)
	}
}

func TestLocalCollectionRetainsCapturedAndDirtySuccessor(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	capture, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if _, err = p.WriteAt(t.Context(), []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = p.WriteAt(t.Context(), []byte{4}, 8); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Collect(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	tree, err := OpenGeneration(t.Context(), p.disk.writer.Source, cfg.Scope, cfg.Keys, capture.Root(), cfg.Base.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tree.ReadBlock(t.Context(), 0)
	if err != nil || b[7] != 2 {
		t.Fatalf("capture lost: %v", err)
	}
	if localByte(t, p) != 3 {
		t.Fatal("successor lost")
	}
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	b = make([]byte, 2)
	if _, err = p.ReadAt(t.Context(), b, 7); err != nil || !bytes.Equal(b, []byte{3, 4}) {
		t.Fatalf("dirty successor lost: %v %v", b, err)
	}
	capture.Release()
	n, err := p.Collect(t.Context(), 1000)
	if err != nil || n == 0 {
		t.Fatalf("released capture not collected: %d %v", n, err)
	}
	if err = capture.Publish(t.Context(), nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("released capture usable: %v", err)
	}
}

func TestLocalCollectionFailedRootCommitRetainsBothRoots(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.WriteAt(t.Context(), []byte{2}, 7)
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.WriteAt(t.Context(), []byte{3}, 7)
	p.phase = func(phase string) error {
		if phase == "root-synced" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err = p.Flush(t.Context()); err == nil {
		t.Fatal("missing failure")
	}
	p.phase = nil
	if _, err = p.Collect(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if localByte(t, p) != 3 {
		t.Fatal("in-memory tree lost")
	}
	p.Close()
	p, err = OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if localByte(t, p) != 2 {
		t.Fatal("acknowledged disk root lost")
	}
}

func TestLocalCollectionFailureAndBudgetDoNotReleaseCredit(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := 1; i < 4; i++ {
		p.WriteAt(t.Context(), []byte{byte(i)}, 7)
		if _, err = p.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	before := p.disk.writer.Sink.(*reservedGenerationSink).remaining
	if _, err = p.Collect(t.Context(), 1); err == nil {
		t.Fatal("ignored graph budget")
	}
	if p.disk.writer.Sink.(*reservedGenerationSink).remaining != before {
		t.Fatal("credited failed traversal")
	}
	p.phase = func(phase string) error {
		if phase == "objects-unlinked" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err = p.Collect(t.Context(), 1000); err == nil {
		t.Fatal("ignored unlink durability error")
	}
	if p.disk.writer.Sink.(*reservedGenerationSink).remaining != before {
		t.Fatal("credited non-durable unlink")
	}
	p.phase = nil
	if _, err = p.Collect(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if p.disk.writer.Sink.(*reservedGenerationSink).remaining <= before {
		t.Fatal("retry did not reclaim credit")
	}
	if localByte(t, p) != 3 {
		t.Fatal("partial collection lost head")
	}
}

func TestLocalCollectionDuringPublication(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	remote := cfg.BaseSource.(*cas.File)
	base := &continuationTestPublication{generationTestPublication: &generationTestPublication{remote: remote, registered: map[string]blockformat.ObjectInspection{}, certified: map[string]blockformat.ObjectInspection{}}}
	loc, _ := cfg.Base.Locator(cfg.Base.LogicalBytes)
	if _, err := publishGeneration(t.Context(), remote, remote, cfg.Scope, cfg.Keys, loc, cfg.Base.LogicalBytes, 1000, base, nil); err != nil {
		t.Fatal(err)
	}
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.WriteAt(t.Context(), []byte{2}, 7)
	capture, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	publisher := &blockedGenerationPublication{continuationTestPublication: base, entered: make(chan struct{}), resume: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- capture.Publish(ctx, publisher) }()
	<-publisher.entered
	p.WriteAt(t.Context(), []byte{3}, 7)
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Collect(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	close(publisher.resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	tree, err := OpenGeneration(t.Context(), remote, cfg.Scope, cfg.Keys, capture.Root(), cfg.Base.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tree.ReadBlock(t.Context(), 0)
	if err != nil || b[7] != 2 {
		t.Fatalf("publication lost: %v", err)
	}
}

func TestLocalCollectionRetainsRootBetweenConsecutivePersistFailures(t *testing.T) {
	for _, afterRename := range []string{"root-renamed", "root-committed"} {
		t.Run(afterRename, func(t *testing.T) {
			cfg, _ := localGenerationFixture(t)
			p, err := CreateLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			p.WriteAt(t.Context(), []byte{2}, 7)
			if _, err = p.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			p.WriteAt(t.Context(), []byte{3}, 7)
			p.phase = func(phase string) error {
				if phase == afterRename {
					return errors.New("after rename")
				}
				return nil
			}
			if _, err = p.Flush(t.Context()); err == nil {
				t.Fatal("missing post-rename failure")
			}
			p.WriteAt(t.Context(), []byte{4}, 7)
			p.phase = func(phase string) error {
				if phase == "root-synced" {
					return errors.New("before rename")
				}
				return nil
			}
			if _, err = p.Flush(t.Context()); err == nil {
				t.Fatal("missing pre-rename failure")
			}
			p.phase = nil
			if _, err = p.Collect(t.Context(), 1000); err != nil {
				t.Fatal(err)
			}
			if localByte(t, p) != 4 {
				t.Fatal("memory root lost")
			}
			p.Close()
			p, err = OpenLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if localByte(t, p) != 3 {
				t.Fatal("installed intermediate root lost")
			}
		})
	}
}
