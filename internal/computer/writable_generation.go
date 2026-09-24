package computer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/ids"
)

var ErrGenerationBufferFull = errors.New("generation dirty buffer is full")
var ErrGenerationStagingFull = errors.New("generation staging reservation is exhausted")

// WritableGeneration overlays a retained authenticated generation. Its caller
// owns the source/key retention and exclusively owns the sink's staging budget.
// Capture freezes a bounded batch while newer writes continue. It produces a
// private immutable root, not a local durable commit or an externally saved head.
// The VM adapter must not acknowledge FLUSH until its local commit also succeeds.
type WritableGeneration struct {
	capture       sync.Mutex
	mu            sync.Mutex
	writer        blockformat.Writer
	tree          *blockformat.Tree
	root          GenerationRoot
	dirty, frozen map[uint64][]byte
	limit         int
	closed        bool
}

type reservedGenerationSink struct {
	target    blockformat.ObjectSink
	remaining int64
}

func (s *reservedGenerationSink) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	if int64(len(raw)) > s.remaining {
		return ErrGenerationStagingFull
	}
	// Charge before I/O: a failed acknowledgement can still leave retained bytes.
	s.remaining -= int64(len(raw))
	return s.target.StoreObject(ctx, digest, raw)
}

// OpenWritableGeneration never creates or repairs a missing source. Source must
// also read objects written to Sink. Limits cover pending plaintext (including a
// frozen batch) and all staged ciphertext attempts for this owner's lifetime.
// Request buffers are capped by MaxReadBytes; tree reads also use bounded
// per-block authentication scratch.
func OpenWritableGeneration(ctx context.Context, writer blockformat.Writer, root GenerationRoot, dirtyBlocks int, stagedBytes int64) (*WritableGeneration, error) {
	if _, err := ids.Parse(writer.ActiveKey); err != nil {
		return nil, errors.New("generation write key identity is invalid")
	}
	if len(writer.Scope) == 0 || len(writer.Scope) > 256 || writer.Source == nil || writer.Sink == nil || dirtyBlocks <= 0 || dirtyBlocks > blockformat.MaxChangedBlocks || stagedBytes <= 0 || len(writer.Keys[writer.ActiveKey]) != 32 || writer.PackLimit < blockformat.MinPackLimit || writer.PackLimit > 4<<20 {
		return nil, errors.New("writable generation requires admitted source, writer and finite budgets")
	}
	keys := make(map[string][]byte, len(writer.Keys))
	for id, key := range writer.Keys {
		keys[id] = bytes.Clone(key)
	}
	writer.Keys = keys
	tree, err := OpenGeneration(ctx, writer.Source, writer.Scope, keys, root, root.LogicalBytes)
	if err != nil {
		for _, key := range keys {
			clear(key)
		}
		return nil, err
	}
	writer.Sink = &reservedGenerationSink{target: writer.Sink, remaining: stagedBytes}
	return &WritableGeneration{writer: writer, tree: tree, root: root, dirty: make(map[uint64][]byte), limit: dirtyBlocks}, nil
}

func (d *WritableGeneration) bounds(offset int64, length int) error {
	if d.closed {
		return os.ErrClosed
	}
	if offset < 0 || length < 0 || length > blockformat.MaxReadBytes || offset > d.root.LogicalBytes-int64(length) {
		return errors.New("writable generation range bounds")
	}
	return nil
}

func (d *WritableGeneration) block(ctx context.Context, index uint64) ([]byte, error) {
	if b, ok := d.dirty[index]; ok {
		return bytes.Clone(b), nil
	}
	if b, ok := d.frozen[index]; ok {
		return bytes.Clone(b), nil
	}
	return d.tree.ReadBlock(ctx, index)
}

func (d *WritableGeneration) ReadAt(ctx context.Context, p []byte, offset int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.bounds(offset, len(p)); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Stage the bounded result so authentication failure never returns partial data.
	result := make([]byte, len(p))
	defer clear(result)
	end := offset + int64(len(p))
	for at := offset; at < end; {
		index := uint64(at / blockformat.BlockSize)
		to := min(end, (int64(index)+1)*blockformat.BlockSize)
		block, changed := d.dirty[index]
		if !changed {
			block, changed = d.frozen[index]
		}
		if changed {
			from := at % blockformat.BlockSize
			copy(result[at-offset:to-offset], block[from:from+to-at])
		} else {
			// Traverse each contiguous unchanged range once, rather than
			// fetching the same index pages separately for every block.
			for to < end {
				next := uint64(to / blockformat.BlockSize)
				if _, ok := d.dirty[next]; ok {
					break
				}
				if _, ok := d.frozen[next]; ok {
					break
				}
				to = min(end, (int64(next)+1)*blockformat.BlockSize)
			}
			data, err := d.tree.ReadRange(ctx, at, int(to-at))
			if err != nil {
				return 0, err
			}
			copy(result[at-offset:to-offset], data)
			clear(data)
		}
		at = to
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return copy(p, result), nil
}

func (d *WritableGeneration) WriteAt(ctx context.Context, p []byte, offset int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeLocked(ctx, p, offset, len(p))
}

// A nil input clears only the requested subranges; callers serialize before
// allocating temporary blocks, including concurrent trim requests.
func (d *WritableGeneration) writeLocked(ctx context.Context, p []byte, offset int64, length int) (int, error) {
	if err := d.bounds(offset, length); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if length == 0 {
		return 0, nil
	}
	first, last := uint64(offset/blockformat.BlockSize), uint64((offset+int64(length)-1)/blockformat.BlockSize)
	additional := 0
	for b := first; b <= last; b++ {
		if _, ok := d.dirty[b]; !ok {
			additional++
		}
	}
	if len(d.dirty)+len(d.frozen)+additional > d.limit {
		return 0, ErrGenerationBufferFull
	}
	changes := make(map[uint64][]byte, additional)
	defer func() {
		for _, block := range changes {
			clear(block)
		}
	}()
	for b := first; b <= last; b++ {
		start := int64(b) * blockformat.BlockSize
		from, to := max(offset, start), min(offset+int64(length), start+blockformat.BlockSize)
		var block []byte
		if from == start && to == start+blockformat.BlockSize {
			block = make([]byte, blockformat.BlockSize)
		} else {
			var err error
			block, err = d.block(ctx, b)
			if err != nil {
				return 0, err
			}
		}
		if p == nil {
			clear(block[from-start : to-start])
		} else {
			copy(block[from-start:to-start], p[from-offset:to-offset])
		}
		changes[b] = block
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for b, block := range changes {
		clear(d.dirty[b])
		d.dirty[b] = block
		delete(changes, b)
	}
	return length, nil
}

// Trim is zeroing with the same atomic request and dirty-budget contract as WriteAt.
func (d *WritableGeneration) Trim(ctx context.Context, offset int64, length int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.writeLocked(ctx, nil, offset, length)
	return err
}

func (d *WritableGeneration) Capture(ctx context.Context) (GenerationRoot, error) {
	d.capture.Lock()
	defer d.capture.Unlock()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return GenerationRoot{}, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		d.mu.Unlock()
		return GenerationRoot{}, err
	}
	if len(d.dirty) == 0 {
		root := d.root
		d.mu.Unlock()
		return root, nil
	}
	d.frozen = d.dirty
	d.dirty = make(map[uint64][]byte)
	frozen, base := d.frozen, d.root
	d.mu.Unlock()
	locator, err := base.Locator(base.LogicalBytes)
	if err == nil {
		locator, err = d.writer.Capture(ctx, locator, base.LogicalBytes, frozen)
	}
	var root GenerationRoot
	var tree *blockformat.Tree
	if err == nil {
		root, err = NewGenerationRoot(locator, base.LogicalBytes)
	}
	if err == nil {
		tree, err = OpenGeneration(ctx, d.writer.Source, d.writer.Scope, d.writer.Keys, root, base.LogicalBytes)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		// Newer foreground writes win; failed capture never loses older dirty data.
		for b, block := range frozen {
			if _, newer := d.dirty[b]; !newer {
				d.dirty[b] = block
			} else {
				clear(block)
			}
		}
		d.frozen = nil
		return GenerationRoot{}, err
	}
	d.root, d.tree = root, tree
	for _, block := range frozen {
		clear(block)
	}
	d.frozen = nil
	return root, nil
}

// Close releases plaintext/key memory after any in-flight capture. It never
// flushes, deletes staged objects or releases the caller's durable source pins.
func (d *WritableGeneration) Close() error {
	d.capture.Lock()
	defer d.capture.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	for _, block := range d.dirty {
		clear(block)
	}
	for _, key := range d.writer.Keys {
		clear(key)
	}
	d.dirty = nil
	return nil
}
