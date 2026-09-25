package computer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func writableFixture(t *testing.T, limit int) (*WritableGeneration, blockformat.Writer, GenerationRoot) {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyID := uuid.NewV7().String()
	writer := blockformat.Writer{Source: store, Sink: store, Scope: "fixture", ActiveKey: keyID, Keys: map[string][]byte{keyID: bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	locator, err = writer.Capture(t.Context(), locator, 1<<20, map[uint64][]byte{0: bytes.Repeat([]byte{1}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	root, err := NewGenerationRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := OpenWritableGeneration(t.Context(), writer, root, limit, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	return disk, writer, root
}

func readWritable(t *testing.T, d *WritableGeneration, offset int64, length int) []byte {
	t.Helper()
	out := make([]byte, length)
	if n, err := d.ReadAt(t.Context(), out, offset); err != nil || n != length {
		t.Fatalf("read %d %v", n, err)
	}
	return out
}

func TestWritableGenerationPartialWritesCaptureAndTrim(t *testing.T) {
	disk, writer, base := writableFixture(t, 8)
	changed := bytes.Repeat([]byte{9}, 17)
	if n, err := disk.WriteAt(t.Context(), changed, 4090); err != nil || n != len(changed) {
		t.Fatalf("partial write %d %v", n, err)
	}
	expected := append(bytes.Repeat([]byte{1}, 4090), changed...)
	expected = append(expected, make([]byte, 8192-len(expected))...)
	if got := readWritable(t, disk, 0, 8192); !bytes.Equal(got, expected) {
		t.Fatal("unaligned write lost source or hole bytes")
	}
	captured, err := disk.Capture(t.Context())
	if err != nil || captured == base {
		t.Fatalf("capture: %v", err)
	}
	tree, err := OpenGeneration(t.Context(), writer.Source, writer.Scope, writer.Keys, captured, captured.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tree.ReadRange(t.Context(), 0, 8192)
	if err != nil || !bytes.Equal(got, expected) {
		t.Fatal("captured data differs", err)
	}
	if err = disk.Trim(t.Context(), 0, 8192); err != nil {
		t.Fatal(err)
	}
	zeroRoot, err := disk.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	zero, err := OpenGeneration(t.Context(), writer.Source, writer.Scope, writer.Keys, zeroRoot, zeroRoot.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, err = zero.ReadRange(t.Context(), 0, 8192)
	if err != nil || !bytes.Equal(got, make([]byte, 8192)) {
		t.Fatal("trim was not captured as holes", err)
	}
	old, err := OpenGeneration(t.Context(), writer.Source, writer.Scope, writer.Keys, captured, captured.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, err = old.ReadRange(t.Context(), 0, 8192)
	if err != nil || !bytes.Equal(got, expected) {
		t.Fatal("later capture mutated fixed root", err)
	}
}

func TestWritableGenerationBoundsAndBackpressure(t *testing.T) {
	disk, writer, root := writableFixture(t, 1)
	before := readWritable(t, disk, 0, 8192)
	if n, err := disk.WriteAt(t.Context(), []byte{9, 9}, 4095); n != 0 || !errors.Is(err, ErrGenerationBufferFull) {
		t.Fatalf("limit accepted %d %v", n, err)
	}
	if got := readWritable(t, disk, 0, 8192); !bytes.Equal(got, before) {
		t.Fatal("rejected request changed disk")
	}
	if _, err := disk.WriteAt(t.Context(), []byte{7}, 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteAt(t.Context(), []byte{8}, 8192); !errors.Is(err, ErrGenerationBufferFull) {
		t.Fatal("dirty capacity not enforced", err)
	}
	if _, err := disk.Capture(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteAt(t.Context(), []byte{8}, 8192); err != nil {
		t.Fatal("captured buffer not released", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := disk.WriteAt(ctx, []byte{0}, 8192); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := readWritable(t, disk, 8192, 1); got[0] != 8 {
		t.Fatal("cancellation changed data")
	}
	tiny, err := OpenWritableGeneration(t.Context(), writer, root, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer tiny.Close()
	if _, err = tiny.WriteAt(t.Context(), []byte{4}, 8192); err != nil {
		t.Fatal(err)
	}
	if _, err = tiny.Capture(t.Context()); !errors.Is(err, ErrGenerationStagingFull) {
		t.Fatal("staging not bounded", err)
	}
	if got := readWritable(t, tiny, 8192, 1); got[0] != 4 {
		t.Fatal("failed capture lost dirty data")
	}
	if _, err = tiny.WriteAt(t.Context(), []byte{1}, root.LogicalBytes); err == nil {
		t.Fatal("out of range write accepted")
	}
	if err = disk.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = disk.Capture(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed capture", err)
	}
	if !bytes.Equal(writer.Keys[writer.ActiveKey], bytes.Repeat([]byte{7}, 32)) {
		t.Fatal("close cleared borrowed key")
	}
}

type gatedGenerationSink struct {
	blockformat.ObjectSink
	entered, release chan struct{}
	fail             bool
}

func (s *gatedGenerationSink) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	if s.entered != nil {
		close(s.entered)
		s.entered = nil
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.fail {
		return errors.New("injected staging failure")
	}
	return s.ObjectSink.StoreObject(ctx, digest, raw)
}

func TestWritableGenerationCaptureKeepsConcurrentWrites(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			_, writer, root := writableFixture(t, 4)
			entered, release := make(chan struct{}), make(chan struct{})
			gate := &gatedGenerationSink{ObjectSink: writer.Sink, entered: entered, release: release, fail: fail}
			writer.Sink = gate
			disk, err := OpenWritableGeneration(t.Context(), writer, root, 4, 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err = disk.WriteAt(t.Context(), []byte{3}, 0); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := disk.Capture(ctx); done <- err }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("capture did not reach gate: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, err = disk.WriteAt(t.Context(), []byte{5}, 1); err != nil {
				t.Fatal(err)
			}
			if _, err = disk.WriteAt(t.Context(), []byte{8}, 4096); err != nil {
				t.Fatal(err)
			}
			close(release)
			err = <-done
			if (err != nil) != fail {
				t.Fatalf("capture error %v", err)
			}
			if got := readWritable(t, disk, 0, 3); !bytes.Equal(got, []byte{3, 5, 1}) {
				t.Fatalf("concurrent boundary lost: %v", got)
			}
			if got := readWritable(t, disk, 4096, 1); got[0] != 8 {
				t.Fatal("new block lost")
			}
			gate.fail = false
			next, err := disk.Capture(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tree, err := OpenGeneration(t.Context(), writer.Source, writer.Scope, writer.Keys, next, next.LogicalBytes)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tree.ReadRange(t.Context(), 0, 3)
			if err != nil || !bytes.Equal(got, []byte{3, 5, 1}) {
				t.Fatal("next capture lost combined writes", err)
			}
		})
	}
}

func TestWritableGenerationRejectsUnpublishableWriteKey(t *testing.T) {
	_, writer, root := writableFixture(t, 4)
	id := uuid.New().String()
	writer.Keys[id] = bytes.Repeat([]byte{3}, 32)
	writer.ActiveKey = id
	if disk, err := OpenWritableGeneration(t.Context(), writer, root, 4, 32<<20); err == nil {
		disk.Close()
		t.Fatal("non-v7 write key accepted")
	}
}

type failingWritableSource struct {
	blockformat.RangeSource
	fail bool
}

func (s *failingWritableSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if s.fail {
		return nil, errors.New("injected authenticated source failure")
	}
	return s.RangeSource.GetRange(ctx, digest, size, offset, length)
}
func TestWritableGenerationSourceFailureIsAtomic(t *testing.T) {
	_, writer, root := writableFixture(t, 4)
	source := &failingWritableSource{RangeSource: writer.Source}
	writer.Source = source
	disk, err := OpenWritableGeneration(t.Context(), writer, root, 4, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if _, err = disk.WriteAt(t.Context(), bytes.Repeat([]byte{4}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	source.fail = true
	if n, err := disk.WriteAt(t.Context(), []byte{9, 9}, 4095); err == nil || n != 0 {
		t.Fatal("partial source failure accepted", n, err)
	}
	if got := readWritable(t, disk, 4095, 1); got[0] != 4 {
		t.Fatal("failed write partially changed overlay")
	}
	out := bytes.Repeat([]byte{7}, 8192)
	if n, err := disk.ReadAt(t.Context(), out, 0); err == nil || n != 0 || !bytes.Equal(out, bytes.Repeat([]byte{7}, 8192)) {
		t.Fatal("failed read exposed partial data", n, err)
	}
	source.fail = false
}
