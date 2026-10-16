package disk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/ids"
)

var ErrVersionBufferFull = errors.New("version dirty buffer is full")
var ErrVersionStagingFull = errors.New("version staging reservation is exhausted")

// WritableVersion overlays a retained authenticated version. Its caller
// owns the source/key retention and exclusively owns the sink's staging budget.
// Capture freezes a bounded batch while newer writes continue. It produces a
// private immutable root, not a local durable commit or an externally saved head.
// The VM adapter must not acknowledge FLUSH until its local commit also succeeds.
type WritableVersion struct {
	budget                     blockformat.CaptureBudget
	pressureSince              time.Time
	pressureActive             atomic.Bool
	life                       sync.RWMutex // Retains keys for unlocked remote reads.
	capture                    sync.Mutex
	mu                         sync.Mutex
	writer                     blockformat.Writer
	tree                       *blockformat.Tree
	root                       VersionRoot
	dirty, frozen              map[uint64][]byte
	limit                      int
	closed                     bool
	generation, rootGeneration uint64
	changedAt, lastChangedAt   time.Time
	changeWake                 chan struct{}
	pressureWake               chan struct{}
}

type reservedVersionSink struct {
	mu                                   sync.Mutex
	dirtyReservation, captureReservation int64
	spaceWake                            chan struct{}
	target                               blockformat.ObjectSink
	remaining                            int64
	charges                              uint64
}

func (s *reservedVersionSink) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	return s.storeObject(ctx, digest, raw, s.target)
}

func (s *reservedVersionSink) storeObject(ctx context.Context, digest [32]byte, raw []byte, target blockformat.ObjectSink) error {
	s.mu.Lock()
	if int64(len(raw)) > s.remaining {
		s.mu.Unlock()
		return ErrVersionStagingFull
	}
	// Charge before I/O: a failed acknowledgement can still leave retained bytes.
	s.remaining -= int64(len(raw))
	s.charges++
	s.captureReservation = max(0, s.captureReservation-int64(len(raw)))
	s.mu.Unlock()
	return target.StoreObject(ctx, digest, raw)
}

// A capture batch must make staged object names durable before the new root
// replaces the frozen generation. The ordinary sink remains individually durable.
type captureObjectBatch interface {
	blockformat.ObjectSink
	Sync(context.Context) error
}

type reservedCaptureSink struct {
	reservation *reservedVersionSink
	target      blockformat.ObjectSink
}

func (s reservedCaptureSink) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	return s.reservation.storeObject(ctx, digest, raw, s.target)
}

// OpenWritableVersion never creates or repairs a missing source. Source must
// also read objects written to Sink. Limits cover pending plaintext (including a
// frozen batch) and all staged ciphertext attempts for this owner's lifetime.
// Request buffers are capped by MaxReadBytes; tree reads also use bounded
// per-block authentication scratch.
func OpenWritableVersion(ctx context.Context, writer blockformat.Writer, root VersionRoot, dirtyBlocks int, stagedBytes int64) (*WritableVersion, error) {
	if _, err := ids.Parse(writer.ActiveKey); err != nil {
		return nil, errors.New("version write key identity is invalid")
	}
	if len(writer.Scope) == 0 || len(writer.Scope) > 256 || writer.Source == nil || writer.Sink == nil || dirtyBlocks <= 0 || dirtyBlocks > blockformat.MaxChangedBlocks || stagedBytes < 0 || len(writer.Keys[writer.ActiveKey]) != 32 || writer.PackLimit < blockformat.MinPackLimit || writer.PackLimit > 4<<20 {
		return nil, errors.New("writable version requires admitted source, writer and finite budgets")
	}
	keys := make(map[string][]byte, len(writer.Keys))
	for id, key := range writer.Keys {
		keys[id] = bytes.Clone(key)
	}
	writer.Keys = keys
	tree, err := OpenVersion(ctx, writer.Source, writer.Scope, keys, root, root.LogicalBytes)
	if err != nil {
		for _, key := range keys {
			clear(key)
		}
		return nil, err
	}
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		return nil, err
	}
	shape, err := blockformat.ReadRoot(ctx, writer.Source, writer.Scope, writer.Keys, locator)
	if err != nil {
		return nil, err
	}
	budget, err := blockformat.NewCaptureBudget(shape, writer.Scope, writer.Keys)
	if err != nil {
		return nil, err
	}
	writer.Sink = &reservedVersionSink{target: writer.Sink, remaining: stagedBytes, spaceWake: make(chan struct{})}
	return &WritableVersion{budget: budget, writer: writer, tree: tree, root: root, dirty: make(map[uint64][]byte), limit: dirtyBlocks, changeWake: make(chan struct{}, 1), pressureWake: make(chan struct{}, 1)}, nil
}

func (d *WritableVersion) bounds(offset int64, length int) error {
	if d.closed {
		return os.ErrClosed
	}
	if offset < 0 || length < 0 || length > blockformat.MaxReadBytes || offset > d.root.LogicalBytes-int64(length) {
		return errors.New("writable version range bounds")
	}
	return nil
}

func (d *WritableVersion) ReadAt(ctx context.Context, p []byte, offset int64) (int, error) {
	d.life.RLock()
	defer d.life.RUnlock()
	lockGuestIO(ctx, &d.mu)
	if err := d.bounds(offset, len(p)); err != nil {
		d.mu.Unlock()
		return 0, err
	}
	// Snapshot the immutable tree and mutable overlay together. Copy only blocks
	// in this request; capture may clear the old overlay as soon as we unlock.
	tree := d.tree
	overlay := make(map[uint64][]byte)
	if len(p) > 0 {
		first, last := uint64(offset/blockformat.BlockSize), uint64((offset+int64(len(p))-1)/blockformat.BlockSize)
		for index := first; index <= last; index++ {
			block, ok := d.dirty[index]
			if !ok {
				block, ok = d.frozen[index]
			}
			if ok {
				overlay[index] = bytes.Clone(block)
			}
		}
	}
	d.mu.Unlock()
	defer func() {
		for _, block := range overlay {
			clear(block)
		}
	}()
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
		block, changed := overlay[index]
		if changed {
			from := at % blockformat.BlockSize
			copy(result[at-offset:to-offset], block[from:from+to-at])
		} else {
			// Traverse each contiguous unchanged range once, rather than
			// fetching the same index pages separately for every block.
			for to < end {
				next := uint64(to / blockformat.BlockSize)
				if _, ok := overlay[next]; ok {
					break
				}
				to = min(end, (int64(next)+1)*blockformat.BlockSize)
			}
			data, err := tree.ReadRange(ctx, at, int(to-at))
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

func (d *WritableVersion) WriteAt(ctx context.Context, p []byte, offset int64) (int, error) {
	return d.write(ctx, p, offset, len(p), true)
}

// Partial boundary blocks are fetched outside the version mutex. Retry if any
// mutation or tree installation changed the snapshot, so a concurrent write is
// never overwritten by stale read-modify-write data.
func (d *WritableVersion) write(ctx context.Context, p []byte, offset int64, length int, reportPressure bool) (int, error) {
	d.life.RLock()
	defer d.life.RUnlock()
	for {
		lockGuestIO(ctx, &d.mu)
		if err := d.bounds(offset, length); err != nil {
			d.mu.Unlock()
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			d.mu.Unlock()
			return 0, err
		}
		if length == 0 {
			d.mu.Unlock()
			return 0, nil
		}
		tree, generation := d.tree, d.generation
		first, last := uint64(offset/blockformat.BlockSize), uint64((offset+int64(length)-1)/blockformat.BlockSize)
		var missing []uint64
		for _, index := range []uint64{first, last} {
			if len(missing) > 0 && missing[0] == index {
				continue
			}
			start := int64(index) * blockformat.BlockSize
			if offset <= start && offset+int64(length) >= start+blockformat.BlockSize {
				continue
			}
			if _, ok := d.dirty[index]; ok {
				continue
			}
			if _, ok := d.frozen[index]; ok {
				continue
			}
			missing = append(missing, index)
		}
		if len(missing) == 0 {
			n, err := d.writeLocked(ctx, p, offset, length, nil, reportPressure)
			d.mu.Unlock()
			return n, err
		}
		d.mu.Unlock()
		blocks := make(map[uint64][]byte, len(missing))
		var readErr error
		for _, index := range missing {
			blocks[index], readErr = tree.ReadBlock(ctx, index)
			if readErr != nil {
				break
			}
		}
		lockGuestIO(ctx, &d.mu)
		stale := d.generation != generation || d.tree != tree
		var n int
		if readErr == nil && !stale {
			n, readErr = d.writeLocked(ctx, p, offset, length, blocks, reportPressure)
		}
		d.mu.Unlock()
		for _, block := range blocks {
			clear(block)
		}
		if readErr != nil || !stale {
			return n, readErr
		}
	}
}

// A nil input clears only the requested subranges; callers serialize before
// allocating temporary blocks, including concurrent trim requests.
func (d *WritableVersion) writeLocked(ctx context.Context, p []byte, offset int64, length int, loaded map[uint64][]byte, reportPressure bool) (int, error) {
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
		return 0, ErrVersionBufferFull
	}
	if err := d.reserveRangeLocked(first, last, reportPressure); err != nil {
		return 0, err
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
			source, ok := d.dirty[b]
			if !ok {
				source, ok = d.frozen[b]
			}
			if !ok {
				source = loaded[b]
			}
			if len(source) != blockformat.BlockSize {
				return 0, errors.New("missing partial write snapshot")
			}
			block = bytes.Clone(source)
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
	d.generation++
	d.lastChangedAt = time.Now()
	if d.changedAt.IsZero() {
		d.changedAt = d.lastChangedAt
	}
	select {
	case d.changeWake <- struct{}{}:
	default:
	}
	return length, nil
}

// Trim is zeroing with the same atomic request and dirty-budget contract as WriteAt.
func (d *WritableVersion) Trim(ctx context.Context, offset int64, length int) error {
	_, err := d.write(ctx, nil, offset, length, true)
	return err
}

func (d *WritableVersion) Capture(ctx context.Context) (VersionRoot, error) {
	return d.captureBoundary(ctx, nil, nil)
}

// cut releases the request boundary after the generation swap, before encoding.
// It also runs on an early rejection so the caller never strands device I/O.
func (d *WritableVersion) captureBoundary(ctx context.Context, cut func(), batch captureObjectBatch) (VersionRoot, error) {
	defer func() {
		if cut != nil {
			cut()
		}
	}()
	lockGuestIO(ctx, &d.capture)
	defer d.capture.Unlock()
	lockGuestIO(ctx, &d.mu)
	if d.closed {
		d.mu.Unlock()
		return VersionRoot{}, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		d.mu.Unlock()
		return VersionRoot{}, err
	}
	if len(d.dirty) == 0 {
		root := d.root
		d.mu.Unlock()
		return root, nil
	}
	sink := d.writer.Sink.(*reservedVersionSink)
	if err := d.reserveRangeLocked(1, 0, true); err != nil {
		d.mu.Unlock()
		return VersionRoot{}, err
	}
	if err := sink.beginCapture(); err != nil {
		d.markPressureLocked()
		d.mu.Unlock()
		return VersionRoot{}, err
	}
	d.frozen = d.dirty
	d.dirty = make(map[uint64][]byte)
	frozen, base := d.frozen, d.root
	generation := d.generation
	d.mu.Unlock()
	if cut != nil {
		cut()
		cut = nil
	}
	writer := d.writer
	if batch != nil {
		writer.Sink = reservedCaptureSink{reservation: sink, target: batch}
	}
	locator, err := base.Locator(base.LogicalBytes)
	if err == nil {
		locator, err = writer.Capture(ctx, locator, base.LogicalBytes, frozen)
	}
	if err == nil && batch != nil {
		// Failure must merge frozen writes back below, before selecting a root.
		err = batch.Sync(ctx)
	}
	var root VersionRoot
	var tree *blockformat.Tree
	if err == nil {
		root, err = NewVersionRoot(locator, base.LogicalBytes)
	}
	if err == nil {
		tree, err = OpenVersion(ctx, d.writer.Source, d.writer.Scope, d.writer.Keys, root, base.LogicalBytes)
	}
	lockGuestIO(ctx, &d.mu)
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
		sink.finishCapture()
		_ = d.reserveRangeLocked(1, 0, true)
		return VersionRoot{}, err
	}
	d.root, d.tree = root, tree
	d.rootGeneration = generation
	for _, block := range frozen {
		clear(block)
	}
	d.frozen = nil
	sink.finishCapture()
	return root, nil
}

// Close releases plaintext/key memory after any in-flight capture. It never
// flushes, deletes staged objects or releases the caller's durable source pins.
func (d *WritableVersion) Close() error {
	d.life.Lock()
	defer d.life.Unlock()
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

// VersionChanges is one atomic view of guest mutations. Wake is a buffered edge;
// callers compare Generation after reading the snapshot and after every wake.
// ChangedAt and LastChangedAt bound mutations since the last publication-owner
// cut. Ordinary guest flushes/local staging captures do not reset this interval.
type VersionChanges struct {
	Generation    uint64
	ChangedAt     time.Time
	LastChangedAt time.Time
	Wake          <-chan struct{}
	PressureSince time.Time
	PressureWake  <-chan struct{}
}

func (d *WritableVersion) Changes() VersionChanges {
	d.mu.Lock()
	defer d.mu.Unlock()
	return VersionChanges{Generation: d.generation, ChangedAt: d.changedAt, LastChangedAt: d.lastChangedAt, Wake: d.changeWake, PressureSince: d.pressureSince, PressureWake: d.pressureWake}
}

// StagingPressure carries the exact space edge sampled by rejected admission,
// so collection between rejection and waiting cannot be missed.
type StagingPressure struct{ Wake <-chan struct{} }

func (*StagingPressure) Error() string { return ErrVersionStagingFull.Error() }
func (*StagingPressure) Unwrap() error { return ErrVersionStagingFull }

func (s *reservedVersionSink) reserveDirty(bytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes+s.captureReservation > s.remaining {
		return &StagingPressure{Wake: s.spaceWake}
	}
	s.dirtyReservation = bytes
	return nil
}
func (s *reservedVersionSink) beginCapture() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirtyReservation > s.remaining {
		return &StagingPressure{Wake: s.spaceWake}
	}
	s.captureReservation, s.dirtyReservation = s.dirtyReservation, 0
	return nil
}
func (s *reservedVersionSink) finishCapture() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureReservation = 0
	close(s.spaceWake)
	s.spaceWake = make(chan struct{})
}
func (d *WritableVersion) markPressureLocked() {
	if !d.pressureSince.IsZero() {
		return
	}
	d.pressureSince = time.Now()
	d.pressureActive.Store(true)
	select {
	case d.pressureWake <- struct{}{}:
	default:
	}
	select {
	case d.changeWake <- struct{}{}:
	default:
	}
}
func (d *WritableVersion) clearPressure() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.pressureSince.IsZero() {
		d.pressureSince = time.Time{}
		d.pressureActive.Store(false)
		select {
		case d.pressureWake <- struct{}{}:
		default:
		}
		select {
		case d.changeWake <- struct{}{}:
		default:
		}
	}
}
func (d *WritableVersion) reserveRangeLocked(first, last uint64, reportPressure bool) error {
	blocks := make([]uint64, 0, len(d.dirty)+d.limit)
	for block := range d.dirty {
		blocks = append(blocks, block)
	}
	for block := first; block <= last; block++ {
		if _, ok := d.dirty[block]; !ok {
			blocks = append(blocks, block)
		}
	}
	budget := d.budget.Bytes(blocks)
	err := d.writer.Sink.(*reservedVersionSink).reserveDirty(budget)
	if err != nil && reportPressure {
		d.markPressureLocked()
	}
	return err
}
func (d *WritableVersion) admitRange(ctx context.Context, offset int64, length int) error {
	lockGuestIO(ctx, &d.mu)
	defer d.mu.Unlock()
	if err := d.bounds(offset, length); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	first, last := uint64(offset/blockformat.BlockSize), uint64((offset+int64(length)-1)/blockformat.BlockSize)
	additional := 0
	for block := first; block <= last; block++ {
		if _, ok := d.dirty[block]; !ok {
			additional++
		}
	}
	if len(d.dirty)+len(d.frozen)+additional > d.limit {
		return ErrVersionBufferFull
	}
	return d.reserveRangeLocked(first, last, true)
}

func (s *reservedVersionSink) chargeGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.charges
}
func (s *reservedVersionSink) creditDeleted(bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining += bytes
	if bytes > 0 {
		close(s.spaceWake)
		s.spaceWake = make(chan struct{})
	}
}
func (s *reservedVersionSink) recount(charges uint64, remaining int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.charges != charges {
		return
	}
	s.remaining = remaining
	close(s.spaceWake)
	s.spaceWake = make(chan struct{})
}
