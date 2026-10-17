//go:build linux || darwin

package disk

import (
	"context"
	"errors"
	"testing"
)

type gatedObjectBatch struct {
	captureObjectBatch
	entered, release chan struct{}
	err              error
}

func (b *gatedObjectBatch) Sync(ctx context.Context) error {
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if b.err != nil {
		return b.err
	}
	return b.captureObjectBatch.Sync(ctx)
}

func TestLocalVersionObjectBarrierPrecedesRootAndPreservesWrites(t *testing.T) {
	for _, mode := range []string{"success", "sync-failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			base := p.head.Root
			for _, at := range []int64{7, 4096 + 7} {
				if _, err := p.WriteAt(t.Context(), []byte{2}, at); err != nil {
					t.Fatal(err)
				}
			}
			factory := p.objectBatch
			gate := &gatedObjectBatch{captureObjectBatch: factory(), entered: make(chan struct{}), release: make(chan struct{})}
			failure := errors.New("directory sync failed")
			if mode == "sync-failure" {
				gate.err = failure
			}
			p.objectBatch = func() captureObjectBatch { return gate }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan captureResult, 1)
			go func() { cut, err := p.Capture(ctx); done <- captureResult{cut, err} }()
			<-gate.entered
			p.disk.mu.Lock()
			selected := p.disk.root
			p.disk.mu.Unlock()
			if selected != base {
				t.Error("root advanced before object directory barrier")
			}
			// New requests run while the frozen capture waits, and must win on retry.
			if _, err := p.WriteAt(t.Context(), []byte{3}, 7); err != nil {
				t.Error(err)
			}
			if mode == "canceled" {
				cancel()
			}
			close(gate.release)
			result := <-done
			if mode == "success" {
				if result.err != nil {
					t.Fatal(result.err)
				}
				tree, err := OpenVersion(t.Context(), p.disk.writer.Source, cfg.Scope, cfg.Keys, result.cut.Root(), cfg.Base.LogicalBytes)
				if err != nil {
					t.Fatal(err)
				}
				data, err := tree.ReadBlock(t.Context(), 0)
				if err != nil || data[7] != 2 {
					t.Fatalf("frozen cut changed: %v", err)
				}
				result.cut.Release()
			} else {
				expected := failure
				if mode == "canceled" {
					expected = context.Canceled
				}
				if !errors.Is(result.err, expected) || result.cut != nil {
					t.Fatalf("failed capture: %+v", result)
				}
				if p.head.Root != base || p.disk.root != base {
					t.Fatal("failed barrier selected or committed root")
				}
			}
			if got := readWritable(t, p.disk, 7, 1)[0]; got != 3 {
				t.Fatalf("newer write = %d", got)
			}
			if got := readWritable(t, p.disk, 4096+7, 1)[0]; got != 2 {
				t.Fatalf("older write = %d", got)
			}
			p.objectBatch = factory
			if _, err := p.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if readWritable(t, reopened.disk, 7, 1)[0] != 3 || readWritable(t, reopened.disk, 4096+7, 1)[0] != 2 {
				t.Fatal("barrier retry lost frozen or successor writes after reopen")
			}
		})
	}
}
