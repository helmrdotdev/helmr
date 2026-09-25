//go:build linux || darwin

package executor

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type saveStoragePublisher struct{ *cas.File }

func (p saveStoragePublisher) Publish(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	return p.Put(ctx, d.MediaType, io.NewSectionReader(f, 0, d.SizeBytes))
}

type collectedSaveCapture struct {
	*computer.LocalCapture
	collected chan int64
}

func (c collectedSaveCapture) Collect(ctx context.Context, budget int) (int64, error) {
	n, err := c.LocalCapture.Collect(ctx, budget)
	if err == nil {
		c.collected <- n
	}
	return n, err
}

// Uses the actual coordinator and encrypted generation store. The CP fixture
// models receipt/retention acknowledgement; it does not prove remote GC policy.
func TestRuntimeComputerSaveLoopReclaimsStagingAcrossSaves(t *testing.T) {
	remote, err := cas.NewFile(filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7().String()
	writer := blockformat.Writer{Source: remote, Sink: remote, Scope: "fixture", ActiveKey: key, Keys: map[string][]byte{key: bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := computer.LocalGenerationConfig{Directory: filepath.Join(t.TempDir(), "local"), Base: root, BaseSource: remote, Scope: writer.Scope, ActiveKey: key, Keys: writer.Keys, DirtyBlocks: 8, StagedBytes: 96 << 10, PackLimit: blockformat.MinPackLimit}
	disk, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	owner := &runtimeComputerSaves{}
	client := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	_, err = owner.attach(client.runtime, client.computer, func() *workerapi.ComputerSaveBeginRequest { return &workerapi.ComputerSaveBeginRequest{} })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time)
	collected := make(chan int64, 1)
	done := make(chan error, 1)
	go func() {
		done <- owner.loop(ctx, ticks, client, saveStoragePublisher{remote}, func(ctx context.Context) (computerSaveCapture, error) {
			capture, err := disk.Capture(ctx)
			if err != nil {
				return nil, err
			}
			return collectedSaveCapture{capture, collected}, nil
		})
	}()
	defer func() { cancel(); <-done; _ = owner.Quiesce(context.Background()) }()
	var total int64
	for i := 1; i <= 32; i++ {
		want := bytes.Repeat([]byte{byte(i)}, 4096)
		offset := int64(i * 4096)
		if _, err = disk.WriteAt(ctx, want, offset); err != nil {
			t.Fatal(err)
		}
		select {
		case ticks <- time.Now():
		case err := <-done:
			done <- err
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("save loop did not accept tick")
		}
		select {
		case n := <-collected:
			total += n
		case err := <-done:
			done <- err
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("adopted save did not reclaim local staging")
		}
		got := make([]byte, len(want))
		if _, err = disk.ReadAt(ctx, got, offset); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("read after collection: %v", err)
		}
	}
	if total <= cfg.StagedBytes {
		t.Fatalf("reclaimed %d bytes, need more than staging allowance %d", total, cfg.StagedBytes)
	}
}
