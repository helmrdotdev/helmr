package computerhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type memoryRanges struct {
	data          []byte
	reads         int
	fail          error
	emptyOnce     bool
	terminalError error
	truncateOnce  bool
}

func (r *memoryRanges) GetRange(ctx context.Context, _ string, size, offset, length int64) (io.ReadCloser, error) {
	r.reads++
	if r.emptyOnce {
		r.emptyOnce = false
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if r.truncateOnce {
		r.truncateOnce = false
		return io.NopCloser(memoryInterruptedReader{}), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.fail != nil {
		return nil, r.fail
	}
	if size != int64(len(r.data)) || offset < 0 || length < 0 || offset > size || length > size-offset {
		return nil, errors.New("bad requested range")
	}
	reader := io.Reader(bytes.NewReader(r.data[offset : offset+length]))
	if r.terminalError != nil {
		reader = io.MultiReader(reader, memoryTerminalError{r.terminalError})
	}
	return io.NopCloser(reader), nil
}
func memoryFixture(t *testing.T, raw []byte) (*CheckpointEncryptor, *memoryRanges, cas.Descriptor, string) {
	t.Helper()
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	header, err := cipher.encryptMemory(t.Context(), bytes.NewReader(raw), &output, "environment/computer/checkpoint/memory", int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	data := output.Bytes()
	return cipher, &memoryRanges{data: data}, cas.Descriptor{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data)), MediaType: cas.CheckpointMemoryMediaType}, header
}
func TestCheckpointMemorySelectiveAuthenticatedReads(t *testing.T) {
	raw := make([]byte, 3*memoryChunkSize+4096)
	copy(raw[memoryChunkSize:2*memoryChunkSize], bytes.Repeat([]byte("compressible"), memoryChunkSize/12+1))
	if _, err := rand.Read(raw[2*memoryChunkSize:]); err != nil {
		t.Fatal(err)
	}
	cipher, ranges, descriptor, header := memoryFixture(t, raw)
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if ranges.reads != 2 {
		t.Fatalf("open fetched payload: %d reads", ranges.reads)
	}
	for _, offset := range []int64{0, memoryChunkSize + 4096, 2 * memoryChunkSize, 3 * memoryChunkSize} {
		got := make([]byte, 4096)
		before := ranges.reads
		n, err := source.ReadAt(t.Context(), got, offset)
		if err != nil || n != len(got) || !bytes.Equal(got, raw[offset:offset+4096]) {
			t.Fatalf("offset %d: n=%d err=%v", offset, n, err)
		}
		if offset == 0 && ranges.reads != before {
			t.Fatal("zero chunk fetched remote payload")
		}
	}
	if source.Size() != int64(len(raw)) {
		t.Fatal("wrong logical size")
	}
}
func TestCheckpointMemoryRejectsTampering(t *testing.T) {
	raw := make([]byte, 2*memoryChunkSize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"header", "header-pin", "index", "payload", "reordered", "purpose", "key", "geometry", "size", "truncated"} {
		t.Run(test, func(t *testing.T) {
			cipher, ranges, descriptor, header := memoryFixture(t, raw)
			purpose := "environment/computer/checkpoint/memory"
			size := int64(len(raw))
			switch test {
			case "header":
				ranges.data[len(memoryMagic)+keyIDSize] ^= 1
			case "header-pin":
				header = sha256sum.DigestBytes([]byte("another attempt"))
			case "index":
				ranges.data[len(ranges.data)-1] ^= 1
			case "payload":
				ranges.data[memoryHeaderSize] ^= 1
			case "reordered":
				length := memoryChunkSize + tagSize
				first := append([]byte(nil), ranges.data[memoryHeaderSize:memoryHeaderSize+length]...)
				copy(ranges.data[memoryHeaderSize:], ranges.data[memoryHeaderSize+length:memoryHeaderSize+2*length])
				copy(ranges.data[memoryHeaderSize+length:], first)
			case "purpose":
				purpose = "other-environment/computer/checkpoint/memory"
			case "key":
				cipher, _ = NewCheckpointEncryptor(bytes.Repeat([]byte{8}, 32))
			case "geometry":
				size += 4096
			case "size":
				descriptor.SizeBytes++
			case "truncated":
				ranges.data = ranges.data[:len(ranges.data)-1]
			}
			source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, purpose, size)
			if test == "payload" || test == "reordered" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = source.ReadAt(t.Context(), make([]byte, 4096), 0)
			}
			if err == nil {
				t.Fatal("accepted altered memory")
			}
			if test == "key" && !errors.Is(err, ErrCheckpointKeyUnavailable) {
				t.Fatalf("wrong key classification: %v", err)
			}
			if test != "key" && test != "size" && test != "truncated" && !errors.Is(err, errInvalidCheckpoint) {
				t.Fatalf("missing integrity classification: %v", err)
			}
		})
	}
}
func TestCheckpointMemoryInputBounds(t *testing.T) {
	for _, size := range []int64{-1, 0, 1, 4097, math.MaxInt64, math.MaxInt64 - 4095} {
		if _, _, err := memoryGeometry(size); err == nil {
			t.Fatalf("accepted size %d", size)
		}
	}
	cipher, _ := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	for _, length := range []int{4095, 4097} {
		if _, err := cipher.encryptMemory(t.Context(), bytes.NewReader(make([]byte, length)), io.Discard, "purpose", 4096); err == nil {
			t.Fatalf("accepted length %d", length)
		}
	}
	_, ranges, descriptor, header := memoryFixture(t, make([]byte, vm.MemoryReadBytes+4096))
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", vm.MemoryReadBytes+4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []struct {
		off int64
		n   int
	}{{-1, 1}, {0, 0}, {vm.MemoryReadBytes - 1, 2}, {vm.MemoryReadBytes + 4096, 1}} {
		if _, err := source.ReadAt(t.Context(), make([]byte, read.n), read.off); err == nil {
			t.Fatalf("accepted read %+v", read)
		}
	}
}
func TestCheckpointMemoryTransportFailureIsNotCorruption(t *testing.T) {
	raw := bytes.Repeat([]byte{5}, 4096)
	cipher, ranges, descriptor, header := memoryFixture(t, raw)
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", 4096)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("object temporarily inaccessible")
	ranges.fail = failure
	_, err = source.ReadAt(t.Context(), make([]byte, 4096), 0)
	if !errors.Is(err, failure) || errors.Is(err, errInvalidCheckpoint) {
		t.Fatalf("wrong transport failure: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = source.ReadAt(ctx, make([]byte, 4096), 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong canceled read: %v", err)
	}
}

// An interrupted response is retryable transport failure, not proof of bad data.
type memoryInterruptedReader struct{}

func (memoryInterruptedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestCheckpointMemoryRetriesInterruptedPayload(t *testing.T) {
	raw := bytes.Repeat([]byte{5}, 4096)
	cipher, ranges, descriptor, header := memoryFixture(t, raw)
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	before := ranges.reads
	ranges.truncateOnce = true
	got := make([]byte, len(raw))
	if _, err = source.ReadAt(t.Context(), got, 0); err != nil {
		t.Fatal(err)
	}
	if ranges.reads != before+2 || !bytes.Equal(got, raw) {
		t.Fatalf("retry reads=%d, expected=%d", ranges.reads, before+2)
	}
}

func TestCheckpointMemoryBatchesAuthenticatedChunks(t *testing.T) {
	raw := make([]byte, 4*memoryChunkSize+4096)
	copy(raw[memoryChunkSize:2*memoryChunkSize], bytes.Repeat([]byte("compressed"), memoryChunkSize/10))
	if _, err := rand.Read(raw[2*memoryChunkSize:]); err != nil {
		t.Fatal(err)
	}
	cipher, ranges, descriptor, header := memoryFixture(t, raw)
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	before := ranges.reads
	got := make([]byte, 4*memoryChunkSize)
	if n, err := source.ReadAt(t.Context(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, raw[:len(got)]) {
		t.Fatal(n, err)
	}
	if ranges.reads != before+1 {
		t.Fatal("window did not use one remote range")
	}
	tail := make([]byte, 4096)
	if n, err := source.ReadAt(t.Context(), tail, int64(len(got))); err != nil || n != len(tail) || !bytes.Equal(tail, raw[len(got):]) {
		t.Fatal(n, err)
	}
	entry := source.index[3*memoryIndexEntrySize : 4*memoryIndexEntrySize]
	ranges.data[binary.BigEndian.Uint64(entry)] ^= 1
	if _, err := source.ReadAt(t.Context(), got, 0); !errors.Is(err, errInvalidCheckpoint) {
		t.Fatal("last-chunk corruption accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before = ranges.reads
	if _, err := source.ReadAt(ctx, got, 0); !errors.Is(err, context.Canceled) || ranges.reads != before {
		t.Fatal("cancelled window read", err)
	}
}

type memoryTerminalError struct{ err error }

func (r memoryTerminalError) Read([]byte) (int, error) { return 0, r.err }
func TestCheckpointMemoryTerminalProbePreservesTransportFailure(t *testing.T) {
	cipher, ranges, descriptor, header := memoryFixture(t, bytes.Repeat([]byte{17}, 4096))
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		ranges.terminalError = failure
		if err := source.readRangeIntoOnce(t.Context(), 0, make([]byte, 1)); !errors.Is(err, failure) || errors.Is(err, errInvalidCheckpoint) {
			t.Fatal("transport failure labelled corruption", err)
		}
	}
}

func TestCheckpointMemoryOpenedSourceRetainsItsKey(t *testing.T) {
	raw := bytes.Repeat([]byte{29}, 4096)
	cipher, ranges, descriptor, header := memoryFixture(t, raw)
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", 4096)
	if err != nil {
		t.Fatal(err)
	}
	// The allocation's derived cipher remains usable after configured key removal.
	clear(cipher.key)
	rotated, err := NewCheckpointEncryptor(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openCheckpointMemory(t.Context(), ranges, rotated, descriptor, header, "environment/computer/checkpoint/memory", 4096); !errors.Is(err, ErrCheckpointKeyUnavailable) {
		t.Fatal(err)
	}
	got := make([]byte, len(raw))
	if n, err := source.ReadAt(t.Context(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, raw) {
		t.Fatal(n, err)
	}
}

func TestCheckpointMemoryRetriesEmptyRangeResponse(t *testing.T) {
	raw := bytes.Repeat([]byte{41}, 4096)
	cipher, ranges, descriptor, header := memoryFixture(t, raw)
	source, err := openCheckpointMemory(t.Context(), ranges, cipher, descriptor, header, "environment/computer/checkpoint/memory", 4096)
	if err != nil {
		t.Fatal(err)
	}
	before := source.MemoryReadStats()
	ranges.emptyOnce = true
	got := make([]byte, len(raw))
	if n, err := source.ReadAt(t.Context(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, raw) {
		t.Fatal(n, err)
	}
	after := source.MemoryReadStats()
	if after.RangeRequests-before.RangeRequests != 2 || after.CiphertextBytes <= before.CiphertextBytes {
		t.Fatal("missing retry metrics", before, after)
	}
}

// streamMemoryRanges observes response lifetime and injects failures after the
// authenticated metadata has been opened, without changing the memory format.
type streamMemoryRanges struct {
	*memoryRanges
	wrap    func(context.Context, io.ReadCloser) io.ReadCloser
	lengths []int64
	closed  int
}

func (r *streamMemoryRanges) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	body, err := r.memoryRanges.GetRange(ctx, digest, size, offset, length)
	if err != nil {
		return nil, err
	}
	r.lengths = append(r.lengths, length)
	if r.wrap != nil {
		body = r.wrap(ctx, body)
	}
	return &streamMemoryBody{ReadCloser: body, close: func() { r.closed++ }}, nil
}

type streamMemoryBody struct {
	io.ReadCloser
	close func()
}

func (b *streamMemoryBody) Close() error { b.close(); return b.ReadCloser.Close() }

type streamMemoryReader struct {
	io.Reader
	io.Closer
}

func openStreamMemoryFixture(t *testing.T, raw []byte) (*checkpointMemorySource, *streamMemoryRanges) {
	t.Helper()
	cipher, ranges, descriptor, header := memoryFixture(t, raw)
	r := &streamMemoryRanges{memoryRanges: ranges}
	s, err := openCheckpointMemory(t.Context(), r, cipher, descriptor, header, "environment/computer/checkpoint/memory", int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	r.lengths = nil
	r.closed = 0
	return s, r
}

func TestCheckpointMemoryStreamMixedWindowsAndBoundedResponses(t *testing.T) {
	raw := make([]byte, 12*vm.MemoryReadBytes+4096)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	clear(raw[:vm.MemoryReadBytes])
	copy(raw[vm.MemoryReadBytes:2*vm.MemoryReadBytes], bytes.Repeat([]byte{42}, vm.MemoryReadBytes))
	s, r := openStreamMemoryFixture(t, raw)
	var borrowed []byte
	consumed := int64(0)
	for offset := int64(0); offset < int64(len(raw)); {
		next, err := s.ReadWindows(t.Context(), offset, func(at int64, data []byte) error {
			if at != consumed || !bytes.Equal(data, raw[at:at+int64(len(data))]) {
				t.Fatalf("bad window at %d", at)
			}
			if len(data) > vm.MemoryReadBytes || cap(data) > vm.MemoryReadBytes {
				t.Fatal("unbounded plaintext")
			}
			if borrowed != nil && &borrowed[0] != &data[0] {
				t.Fatal("stream did not reuse window")
			}
			borrowed = data
			consumed += int64(len(data))
			return nil
		})
		if err != nil || next <= offset {
			t.Fatalf("next %d err %v", next, err)
		}
		if !bytes.Equal(borrowed, make([]byte, len(borrowed))) {
			t.Fatal("borrowed plaintext not cleared")
		}
		borrowed = nil
		offset = next
	}
	if consumed != int64(len(raw)) || len(r.lengths) < 2 || r.closed != len(r.lengths) {
		t.Fatalf("incomplete stream: %d, %+v", consumed, r)
	}
	for _, n := range r.lengths {
		if n > memoryStreamBytes {
			t.Fatalf("unbounded range %d", n)
		}
	}
	if s.MemoryReadStats().BackgroundRangeRequests != int64(len(r.lengths)) {
		t.Fatal("missing background requests")
	}
}

func TestCheckpointMemoryStreamZeroWindowsNeedNoRequest(t *testing.T) {
	s, r := openStreamMemoryFixture(t, make([]byte, 11*vm.MemoryReadBytes))
	count := 0
	consume := func(_ int64, data []byte) error {
		count++
		if !bytes.Equal(data, make([]byte, len(data))) {
			t.Fatal("nonzero")
		}
		return nil
	}
	next, err := s.ReadWindows(t.Context(), 0, consume)
	if err != nil || next != memoryStreamBytes || count != 8 || len(r.lengths) != 0 {
		t.Fatalf("next=%d count=%d requests=%d err=%v", next, count, len(r.lengths), err)
	}
	next, err = s.ReadWindows(t.Context(), next, consume)
	if err != nil || next != s.Size() || count != 11 || len(r.lengths) != 0 {
		t.Fatalf("remaining zero stream: next=%d count=%d err=%v", next, count, err)
	}
}

func TestCheckpointMemoryStreamInterruptedWindowResumesAtBoundary(t *testing.T) {
	raw := make([]byte, 3*vm.MemoryReadBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	s, r := openStreamMemoryFixture(t, raw)
	_, firstEnd := s.windowRange(0, vm.MemoryReadBytes)
	firstStart, _ := s.windowRange(0, vm.MemoryReadBytes)
	r.wrap = func(_ context.Context, body io.ReadCloser) io.ReadCloser {
		return &streamMemoryReader{Reader: io.LimitReader(body, firstEnd-firstStart+10), Closer: body}
	}
	count := 0
	consume := func(at int64, data []byte) error {
		if at != int64(count*vm.MemoryReadBytes) || !bytes.Equal(data, raw[at:at+int64(len(data))]) {
			t.Fatal("replayed or corrupt window")
		}
		count++
		return nil
	}
	next, err := s.ReadWindows(t.Context(), 0, consume)
	if !errors.Is(err, io.ErrUnexpectedEOF) || next != vm.MemoryReadBytes || count != 1 || r.closed != 1 {
		t.Fatalf("next=%d count=%d close=%d err=%v", next, count, r.closed, err)
	}
	r.wrap = nil
	next, err = s.ReadWindows(t.Context(), next, consume)
	if err != nil || next != s.Size() || count != 3 || r.closed != 2 {
		t.Fatalf("next=%d count=%d close=%d err=%v", next, count, r.closed, err)
	}
}

func TestCheckpointMemoryStreamRejectsCorruptionAndInvalidTail(t *testing.T) {
	for _, kind := range []string{"corrupt", "extra", "close"} {
		t.Run(kind, func(t *testing.T) {
			raw := bytes.Repeat([]byte{19}, 2*vm.MemoryReadBytes)
			s, r := openStreamMemoryFixture(t, raw)
			sentinel := errors.New("close failed")
			switch kind {
			case "corrupt":
				at, _ := s.windowRange(2*vm.MemoryReadBytes-memoryChunkSize, memoryChunkSize)
				r.data[at] ^= 1
			case "extra":
				r.wrap = func(_ context.Context, b io.ReadCloser) io.ReadCloser {
					return &streamMemoryReader{Reader: io.MultiReader(b, bytes.NewReader([]byte{1})), Closer: b}
				}
			case "close":
				r.wrap = func(_ context.Context, b io.ReadCloser) io.ReadCloser {
					return &streamCloseError{ReadCloser: b, err: sentinel}
				}
			}
			count := 0
			next, err := s.ReadWindows(t.Context(), 0, func(_ int64, _ []byte) error { count++; return nil })
			if err == nil || count != 1 || next != vm.MemoryReadBytes || r.closed != 1 {
				t.Fatalf("next=%d count=%d close=%d err=%v", next, count, r.closed, err)
			}
			if kind != "close" && !errors.Is(err, vm.ErrInvalidCheckpointContent) {
				t.Fatalf("not integrity failure: %v", err)
			}
			if kind == "close" && !errors.Is(err, sentinel) {
				t.Fatalf("lost close error: %v", err)
			}
		})
	}
}

type streamCloseError struct {
	io.ReadCloser
	err error
}

func (b *streamCloseError) Close() error { return errors.Join(b.ReadCloser.Close(), b.err) }

func TestCheckpointMemoryStreamCancellationClosesBodyAndClearsBuffer(t *testing.T) {
	s, r := openStreamMemoryFixture(t, bytes.Repeat([]byte{23}, 3*vm.MemoryReadBytes))
	ctx, cancel := context.WithCancel(t.Context())
	var borrowed []byte
	next, err := s.ReadWindows(ctx, 0, func(_ int64, data []byte) error { borrowed = data; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || next != vm.MemoryReadBytes || r.closed != 1 {
		t.Fatalf("next=%d close=%d err=%v", next, r.closed, err)
	}
	if !bytes.Equal(borrowed, make([]byte, len(borrowed))) {
		t.Fatal("canceled plaintext retained")
	}
}

type streamContextReader struct {
	ctx context.Context
	io.Closer
}

func (r *streamContextReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }
func TestCheckpointMemoryStreamBlockedBodyUsesResponseDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r := openStreamMemoryFixture(t, bytes.Repeat([]byte{73}, 2*vm.MemoryReadBytes))
		r.wrap = func(ctx context.Context, b io.ReadCloser) io.ReadCloser {
			return &streamContextReader{ctx: ctx, Closer: b}
		}
		began := time.Now()
		next, err := s.ReadWindows(t.Context(), 0, func(int64, []byte) error { t.Fatal("delivered incomplete window"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) || next != 0 || r.closed != 1 || time.Since(began) != memoryReadTimeout {
			t.Fatalf("next=%d close=%d elapsed=%s err=%v", next, r.closed, time.Since(began), err)
		}
	})
}
func TestCheckpointMemoryStreamDeadlineIsSharedAcrossWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r := openStreamMemoryFixture(t, bytes.Repeat([]byte{74}, 3*vm.MemoryReadBytes))
		count := 0
		next, err := s.ReadWindows(t.Context(), 0, func(int64, []byte) error { count++; time.Sleep(memoryReadTimeout); return nil })
		if !errors.Is(err, context.DeadlineExceeded) || next != vm.MemoryReadBytes || count != 1 || r.closed != 1 {
			t.Fatalf("next=%d count=%d close=%d err=%v", next, count, r.closed, err)
		}
	})
}

func TestCheckpointMemoryStreamingJobsFitReservation(t *testing.T) {
	// Mix a partially compressed chunk with three raw chunks per window to
	// require both the decoder/scratch and large ciphertext buffers.
	raw := make([]byte, 12*vm.MemoryReadBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	for at := 0; at < len(raw); at += vm.MemoryReadBytes {
		clear(raw[at : at+64*1024])
	}
	s, _ := openStreamMemoryFixture(t, raw)
	if s.index[12] != 2 {
		t.Fatal("fixture must allocate a compressed decoder")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// Count all allocations across three complete demand jobs and one stream,
	// even buffers already released. This is stricter than their live overlap;
	// fixture storage and authenticated metadata are funded separately.
	for i := 0; i < 3; i++ {
		buffer := make([]byte, vm.MemoryReadBytes)
		if _, err := s.ReadAt(t.Context(), buffer, int64(i*vm.MemoryReadBytes)); err != nil {
			t.Fatal(err)
		}
		clear(buffer)
	}
	if _, err := s.ReadWindows(t.Context(), 0, func(int64, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 16<<20 {
		t.Fatalf("four memory jobs allocated %d bytes, exceeds 16MiB buffer allowance", allocated)
	}
	t.Logf("three demand jobs plus streamed segment allocated %d bytes in total", allocated)
	runtime.KeepAlive(s)
}
