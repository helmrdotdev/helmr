//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type countedContinuation struct {
	*continuationTestPublication
	registers, certifies, uploads int
	mu                            sync.Mutex
}

func (p *countedContinuation) RegisterBatch(ctx context.Context, i []blockformat.ObjectInspection) error {
	p.registers++
	return p.continuationTestPublication.RegisterBatch(ctx, i)
}
func (p *countedContinuation) CertifyBatch(ctx context.Context, i []blockformat.ObjectInspection) error {
	p.certifies++
	return p.continuationTestPublication.CertifyBatch(ctx, i)
}
func (p *countedContinuation) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	p.mu.Lock()
	p.uploads++
	p.mu.Unlock()
	return p.continuationTestPublication.Upload(ctx, d, file)
}

func TestLocalCertifiedCutReusesObjectsStillPresentLocally(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := &countedContinuation{continuationTestPublication: adoptionPublisher(t, cfg)}
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{8}, 8192), 0); err != nil {
		t.Fatal(err)
	}
	capture, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if err = capture.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if err = capture.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := [3]int{pub.registers, pub.certifies, pub.uploads}
	if before[0] == 0 || before[2] == 0 {
		t.Fatal("initial publication did no work")
	}
	if err = capture.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if got := [3]int{pub.registers, pub.certifies, pub.uploads}; got != before {
		t.Fatalf("certified local bytes were republished: %v -> %v", before, got)
	}
	root := capture.Root()
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := versionObjectLocal(t.Context(), p.store, locator.Pack.Digest, locator.Pack.Size); err != nil || !exists {
		t.Fatal("test lost local bytes", err)
	}
	if n, err := capture.Collect(t.Context(), 1000); n != 0 || err != nil {
		t.Fatalf("unpressured save evicted cache: %d %v", n, err)
	}
}

func TestLocalCollectionTraversalAllowsConcurrentCapture(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	gate := &pausedWritableSource{RangeSource: cfg.BaseSource, entered: make(chan struct{}), release: make(chan struct{})}
	cfg.BaseSource = gate
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	p.reads.clear()
	gate.pause.Store(true)
	collected := make(chan error, 1)
	go func() { _, err := p.Collect(t.Context(), 1000); collected <- err }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("collection did not traverse remote boundary")
	}
	captured := make(chan error, 1)
	go func() {
		if _, err := p.WriteAt(t.Context(), bytes.Repeat([]byte{9}, 4096), 0); err != nil {
			captured <- err
			return
		}
		c, err := p.Capture(t.Context())
		if c != nil {
			c.Release()
		}
		captured <- err
	}()
	select {
	case err := <-captured:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collection traversal held commit across remote I/O")
	}
	release.Do(func() { close(gate.release) })
	if err := <-collected; err != nil {
		t.Fatal(err)
	}
	if got := make([]byte, 4096); func() bool {
		_, err := p.ReadAt(t.Context(), got, 0)
		return err == nil && bytes.Equal(got, bytes.Repeat([]byte{9}, 4096))
	}() == false {
		t.Fatal("collection lost concurrent captured bytes")
	}
}

type measuredRangeSource struct {
	blockformat.RangeSource
	calls, bytes int64
}

func (s *measuredRangeSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	s.calls++
	s.bytes += length
	return s.RangeSource.GetRange(ctx, digest, size, offset, length)
}

// Counts real immutable objects and remote ranges; file-CAS elapsed time is not
// an S3 latency result. Saved cuts intentionally remain retained for this lease.
func TestLocalVersionOverwriteAndRestoredReadMeasurements(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	cfg.DirtyBlocks = 1024
	cfg.StagedBytes = 64 << 20
	pub := &countedContinuation{continuationTestPublication: adoptionPublisher(t, cfg)}
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var final VersionRoot
	var priorObjects int
	for cut := 1; cut <= 16; cut++ {
		content := bytes.Repeat([]byte{byte(cut)}, 1<<20)
		if _, err := p.WriteAt(t.Context(), content, 0); err != nil {
			t.Fatal(err)
		}
		c, err := p.Capture(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Publish(t.Context(), pub); err != nil {
			c.Release()
			t.Fatal(err)
		}
		if err := c.Adopt(t.Context()); err != nil {
			c.Release()
			t.Fatal(err)
		}
		final = c.Root()
		c.Release()
		if cut%4 == 0 {
			var retained int64
			for _, e := range pub.certified {
				retained += inspectionDescriptor(e).SizeBytes
			}
			objects := len(pub.certified)
			t.Logf("cuts=%d guest_written_mib=%d retained_objects=%d retained_bytes=%d register_batches=%d certify_batches=%d puts=%d", cut, cut, objects, retained, pub.registers, pub.certifies, pub.uploads)
			if objects <= priorObjects {
				t.Fatal("fixture did not retain distinct overwritten versions")
			}
			priorObjects = objects
		}
	}
	source := &measuredRangeSource{RangeSource: cfg.BaseSource}
	cfg.Directory = filepath.Join(t.TempDir(), "restored")
	cfg.Base, cfg.BaseSource = final, source
	restored, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for pass := 1; pass <= 2; pass++ {
		calls, fetched := source.calls, source.bytes
		for offset := int64(0); offset < 1<<20; offset += 4096 {
			content := make([]byte, 4096)
			if _, err := restored.ReadAt(t.Context(), content, offset); err != nil || !bytes.Equal(content, bytes.Repeat([]byte{16}, len(content))) {
				t.Fatalf("restored read at %d: %v", offset, err)
			}
		}
		t.Logf("restored_read_pass=%d guest_read_mib=1 remote_gets=%d remote_bytes=%d", pass, source.calls-calls, source.bytes-fetched)
		if pass == 2 && source.calls != calls {
			t.Fatal("bounded repeated read did not reuse ciphertext cache")
		}
	}
}

type parallelBatchPublication struct {
	*continuationTestPublication
	started chan struct{}
	release chan struct{}
	active  atomic.Int32
	maximum atomic.Int32
	loseAck bool
}

func (p *parallelBatchPublication) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for old := p.maximum.Load(); active > old; old = p.maximum.Load() {
		if p.maximum.CompareAndSwap(old, active) {
			break
		}
	}
	p.started <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
		return cas.Object{}, ctx.Err()
	}
	return p.continuationTestPublication.Upload(ctx, d, file)
}
func (p *parallelBatchPublication) CertifyBatch(ctx context.Context, batch []blockformat.ObjectInspection) error {
	if err := p.continuationTestPublication.CertifyBatch(ctx, batch); err != nil {
		return err
	}
	if p.loseAck && len(batch) > 1 {
		p.loseAck = false
		return errors.New("batch committed but acknowledgement lost")
	}
	return nil
}
func TestLocalPublicationBoundsParallelUploadsAndReconcilesBatchAcknowledgement(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Distinct flushed segments in one dependency level.
	for i := 0; i < 16; i++ {
		if _, err := p.WriteAt(t.Context(), bytes.Repeat([]byte{byte(i + 2)}, 4096), int64(i*4096)); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	c, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	pub := &parallelBatchPublication{continuationTestPublication: adoptionPublisher(t, cfg), started: make(chan struct{}, 1000), release: make(chan struct{}), loseAck: true}
	var release sync.Once
	defer release.Do(func() { close(pub.release) })
	result := make(chan error, 1)
	go func() { result <- c.Publish(t.Context(), pub) }()
	for range versionUploadConcurrency {
		select {
		case <-pub.started:
		case <-time.After(5 * time.Second):
			t.Fatal("independent uploads did not overlap")
		}
	}
	if pub.maximum.Load() != versionUploadConcurrency {
		t.Fatal("upload concurrency escaped its bound")
	}
	release.Do(func() { close(pub.release) })
	if err := <-result; err == nil {
		t.Fatal("lost batch acknowledgement accepted")
	}
	for _, e := range pub.certified {
		if e.Segment != nil {
			if _, remote, ok := p.evidence.get(e.Segment.Digest); ok && remote {
				t.Fatal("unacknowledged batch cached as certified")
			}
		}
	}
	if err := c.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if pub.maximum.Load() > versionUploadConcurrency {
		t.Fatal("retry escaped upload bound")
	}
	if err := c.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		data := make([]byte, 4096)
		if _, err := p.ReadAt(t.Context(), data, int64(i*4096)); err != nil || !bytes.Equal(data, bytes.Repeat([]byte{byte(i + 2)}, 4096)) {
			t.Fatalf("retry changed captured bytes: %v", err)
		}
	}
}

func TestLocalCollectionWaitsForInflightGuestRead(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	gate := &pausedWritableSource{RangeSource: cfg.BaseSource, entered: make(chan struct{}), release: make(chan struct{})}
	cfg.BaseSource = gate
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	if _, err = p.WriteAt(t.Context(), []byte{9}, 8192); err != nil {
		t.Fatal(err)
	}
	capture, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if err = capture.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if err = capture.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Fill inspection evidence before stopping a guest read at its remote frame.
	if _, err = p.localReachable(t.Context(), []VersionRoot{p.head.Root, p.head.Base}, 1000); err != nil {
		t.Fatal(err)
	}
	p.reads.clear()
	gate.pause.Store(true)
	read := make(chan error, 1)
	go func() { _, err := p.ReadAt(t.Context(), make([]byte, 4096), 0); read <- err }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("guest read did not reach remote source")
	}
	if n, err := p.Collect(t.Context(), 1000); err != nil || n != 0 {
		t.Fatalf("collection crossed active reader: %d %v", n, err)
	}
	gate.pause.Store(false)
	release.Do(func() { close(gate.release) })
	if err = <-read; err != nil {
		t.Fatal(err)
	}
	if n, err := p.Collect(t.Context(), 1000); err != nil || n == 0 {
		t.Fatalf("collection did not resume after reader: %d %v", n, err)
	}
	got := make([]byte, 1)
	if _, err = p.ReadAt(t.Context(), got, 8192); err != nil || got[0] != 9 {
		t.Fatal("collection changed saved guest bytes")
	}
}
