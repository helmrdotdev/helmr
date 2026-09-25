//go:build linux || darwin

package computer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

type continuationTestPublication struct {
	*generationTestPublication
	reused      int
	rejectReuse bool
}

func (p *continuationTestPublication) Reuse(ctx context.Context, e blockformat.ObjectInspection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.rejectReuse {
		return errors.New("source authority revoked")
	}
	prior, ok := p.certified[inspectionDescriptor(e).Digest]
	if !ok || !reflect.DeepEqual(prior, e) {
		return errors.New("referenced object is not certified")
	}
	p.reused++
	return nil
}

func TestLocalGenerationPublicationRetainsOldSnapshotAcrossNewFlush(t *testing.T) {
	for _, failure := range []string{"", "register", "upload", "certify", "reuse"} {
		t.Run(failure, func(t *testing.T) {
			cfg, _ := localGenerationFixture(t)
			remote := cfg.BaseSource.(*cas.File)
			publisher := &continuationTestPublication{generationTestPublication: &generationTestPublication{remote: remote, registered: map[string]blockformat.ObjectInspection{}, certified: map[string]blockformat.ObjectInspection{}}}
			locator, err := cfg.Base.Locator(cfg.Base.LogicalBytes)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = publishGeneration(t.Context(), remote, remote, cfg.Scope, cfg.Keys, locator, cfg.Base.LogicalBytes, 1000, publisher, nil); err != nil {
				t.Fatal(err)
			}
			// Retained bytes use one key; new writes use another.
			cfg.ActiveKey = uuid.NewV7().String()
			cfg.Keys[cfg.ActiveKey] = bytes.Repeat([]byte{9}, 32)
			disk, err := CreateLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			if _, err = disk.WriteAt(t.Context(), []byte{8}, 4096); err != nil {
				t.Fatal(err)
			}
			capture, err := disk.Capture(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer capture.Release()
			saved := capture.Root()
			publisher.fail = failure
			publisher.rejectReuse = failure == "reuse"
			err = capture.Publish(t.Context(), publisher)
			if failure != "" && err == nil {
				t.Fatal("uncertain publication succeeded")
			}
			if failure == "" && err != nil {
				t.Fatal(err)
			}
			publisher.fail = ""
			publisher.rejectReuse = false
			// A newer local head must not change the previously selected snapshot or
			// prevent retrying its exact ciphertext after an uncertain response.
			if _, err = disk.WriteAt(t.Context(), []byte{9}, 4096); err != nil {
				t.Fatal(err)
			}
			if _, err = disk.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = capture.Publish(t.Context(), publisher); err != nil {
				t.Fatal(err)
			}
			if publisher.reused == 0 {
				t.Fatal("no retained source reuse")
			}
			tree, err := OpenGeneration(t.Context(), remote, cfg.Scope, cfg.Keys, saved, saved.LogicalBytes)
			if err != nil {
				t.Fatal(err)
			}
			old, err := tree.ReadBlock(t.Context(), 0)
			if err != nil || old[0] != 1 {
				t.Fatalf("base: %v %v", old, err)
			}
			got, err := tree.ReadBlock(t.Context(), 1)
			if err != nil || got[0] != 8 {
				t.Fatalf("saved bytes changed: %v %v", got, err)
			}
			savedLocator, err := saved.Locator(saved.LogicalBytes)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = publishGeneration(t.Context(), disk.store, disk.disk.writer.Source, cfg.Scope, cfg.Keys, savedLocator, saved.LogicalBytes, 1, publisher, publisher); err == nil {
				t.Fatal("object budget not enforced")
			}
		})
	}
}

type blockedGenerationPublication struct {
	*continuationTestPublication
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (p *blockedGenerationPublication) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.resume:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.continuationTestPublication.Register(ctx, e)
}

func TestLocalGenerationPublicationAllowsConcurrentFlush(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	remote := cfg.BaseSource.(*cas.File)
	base := &continuationTestPublication{generationTestPublication: &generationTestPublication{remote: remote, registered: map[string]blockformat.ObjectInspection{}, certified: map[string]blockformat.ObjectInspection{}}}
	locator, err := cfg.Base.Locator(cfg.Base.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishGeneration(t.Context(), remote, remote, cfg.Scope, cfg.Keys, locator, cfg.Base.LogicalBytes, 1000, base, nil); err != nil {
		t.Fatal(err)
	}
	disk, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if _, err = disk.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	capture, err := disk.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	saved := capture.Root()
	publisher := &blockedGenerationPublication{continuationTestPublication: base, entered: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { done <- capture.Publish(ctx, publisher) }()
	select {
	case <-publisher.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Complete a newer durable local root while publication is blocked on I/O.
	if _, err = disk.WriteAt(ctx, []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	latest, err := disk.Flush(ctx)
	if err != nil || latest == saved {
		t.Fatalf("flush blocked or unchanged: %v", err)
	}
	close(publisher.resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	tree, err := OpenGeneration(ctx, remote, cfg.Scope, cfg.Keys, saved, saved.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	data, err := tree.ReadBlock(ctx, 0)
	if err != nil || data[7] != 2 {
		t.Fatalf("snapshot changed: %v", err)
	}
}

func TestLocalGenerationPublicationRejectsMissingUnpublishedSegment(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	remote := cfg.BaseSource.(*cas.File)
	publisher := &continuationTestPublication{generationTestPublication: &generationTestPublication{remote: remote, registered: map[string]blockformat.ObjectInspection{}, certified: map[string]blockformat.ObjectInspection{}}}
	locator, err := cfg.Base.Locator(cfg.Base.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishGeneration(t.Context(), remote, remote, cfg.Scope, cfg.Keys, locator, cfg.Base.LogicalBytes, 1000, publisher, nil); err != nil {
		t.Fatal(err)
	}
	disk, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if _, err = disk.WriteAt(t.Context(), []byte{8}, 4096); err != nil {
		t.Fatal(err)
	}
	capture, err := disk.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	saved := capture.Root()
	locator, err = saved.Locator(saved.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	var removeSegment func(blockformat.PackRef)
	removeSegment = func(ref blockformat.PackRef) {
		inspected, e := blockformat.InspectPack(t.Context(), disk.disk.writer.Source, cfg.Scope, cfg.Keys, ref)
		if e != nil {
			t.Fatal(e)
		}
		for _, page := range inspected.Pages {
			for _, segment := range page.Segments {
				d := inspectionDescriptor(blockformat.ObjectInspection{Segment: &segment})
				file, e := disk.store.OpenImmutable(t.Context(), d)
				if errors.Is(e, os.ErrNotExist) {
					continue
				}
				if e != nil {
					t.Fatal(e)
				}
				path := file.Name()
				if e = file.Close(); e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(path); e != nil {
					t.Fatal(e)
				}
				removed = true
				return
			}
			for _, child := range page.Children {
				removeSegment(child.Locator.Pack)
				if removed {
					return
				}
			}
		}
	}
	removeSegment(locator.Pack)
	if !removed {
		t.Fatal("fixture did not contain local segment")
	}
	if err = capture.Publish(t.Context(), publisher); err == nil {
		t.Fatal("missing unpublished bytes treated as retained")
	}
	if _, ok := publisher.certified[saved.Pack.Digest]; ok {
		t.Fatal("incomplete root certified")
	}
}
