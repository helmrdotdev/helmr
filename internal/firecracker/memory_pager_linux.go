//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

const memoryPageBytes = 4096
const memoryFaultDeadline = 20 * time.Second

var errMemoryPager = errors.New("memory paging failed")

type memoryRegion struct {
	Base               uint64 `json:"base_host_virt_addr"`
	Size               uint64 `json:"size"`
	Offset             uint64 `json:"offset"`
	PageSize           uint64 `json:"page_size"`
	DeprecatedPageSize uint64 `json:"page_size_kib"`
}
type memoryChunkState struct {
	loaded              bool
	background          bool
	loading             chan struct{}
	observedDemandPages [vm.MemoryReadBytes / memoryPageBytes / 64]uint64
	observedDemand      bool
}
type memoryFault struct {
	chunk    int64
	received time.Time
	deadline time.Time
}

// One pager owns one physical VMM's UFFD and its authenticated memory source.
// Three demand workers and one background reader bound memory I/O independently
// of disk. Background work never queues ahead of faults or owns their deadline.
type memoryPager struct {
	ctx                        context.Context
	cancel                     context.CancelFunc
	stopVMM                    context.CancelFunc
	fd                         int
	source                     vm.MemorySource
	regions                    []memoryRegion
	workers                    sync.WaitGroup
	background                 sync.Once
	done                       chan struct{}
	faults                     chan memoryFault
	mu                         sync.Mutex
	demandActive               int
	demandDone                 chan struct{}
	chunks                     []memoryChunkState
	err                        error
	closeOnce                  sync.Once
	closeErr                   error
	recordPhase                func(vm.Phase)
	demandDurationBuckets      [32]atomic.Int64
	joinDurationBuckets        [32]atomic.Int64
	faultCount                 atomic.Int64
	chunkCount                 atomic.Int64
	demandLoads                atomic.Int64
	backgroundLoads            atomic.Int64
	backgroundDiscardedWindows atomic.Int64
	backgroundDemandWaitNanos  atomic.Int64
	demandLoadErrors           atomic.Int64
	backgroundLoadErrors       atomic.Int64
	joinBackground             atomic.Int64
	joinDemand                 atomic.Int64
	joinBackgroundNanos        atomic.Int64
	joinDemandNanos            atomic.Int64
	faultQueueNanos            atomic.Int64
	faultServiceNanos          atomic.Int64
	demandReadNanos            atomic.Int64
	backgroundReadNanos        atomic.Int64
	demandPopulateNanos        atomic.Int64
	backgroundPopulateNanos    atomic.Int64
	observedDemandPages        atomic.Int64
	observedDemandWindows      atomic.Int64
	started                    time.Time
	faultObservations          []vm.MemoryFaultObservation
	droppedFaultObservations   int64
}

func validateMemoryRegions(regions []memoryRegion, size int64) error {
	if size <= 0 || size%memoryPageBytes != 0 || len(regions) == 0 || len(regions) > 64 {
		return errors.New("invalid memory region geometry")
	}
	slices.SortFunc(regions, func(a, b memoryRegion) int {
		if a.Offset < b.Offset {
			return -1
		}
		if a.Offset > b.Offset {
			return 1
		}
		return 0
	})
	var offset uint64
	for i, r := range regions {
		if r.PageSize != memoryPageBytes || r.DeprecatedPageSize != 0 && r.DeprecatedPageSize != memoryPageBytes || r.Base == 0 || r.Base%memoryPageBytes != 0 || r.Size == 0 || r.Size%memoryPageBytes != 0 || r.Offset != offset || r.Size > uint64(size)-offset || r.Base > math.MaxUint64-r.Size {
			return errors.New("unsupported memory region mapping")
		}
		for _, prior := range regions[:i] {
			if r.Base < prior.Base+prior.Size && prior.Base < r.Base+r.Size {
				return errors.New("overlapping memory regions")
			}
		}
		offset += r.Size
	}
	if offset != uint64(size) {
		return errors.New("memory regions differ from admitted size")
	}
	return nil
}

// Firecracker sends one JSON value with an fd and keeps the socket open. Never
// wait for EOF. Reject all other peers before accepting any descriptor.
func receiveMemoryHandshake(ctx context.Context, listener *net.UnixListener, pid int, size int64) (int, []memoryRegion, error) {
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := listener.SetDeadline(deadline); err != nil {
		return -1, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	conn, err := listener.AcceptUnix()
	if err != nil {
		return -1, nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return -1, nil, err
	}
	cancelConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer cancelConn()
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, nil, err
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err = raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return -1, nil, err
	}
	if credentialErr != nil {
		return -1, nil, credentialErr
	}
	if credentials == nil || int(credentials.Pid) != pid {
		return -1, nil, errors.New("memory pager peer is not the owned VMM")
	}
	const limit = 64 << 10
	data := make([]byte, limit)
	control := make([]byte, unix.CmsgSpace(4*16))
	n, oobn, flags, _, err := conn.ReadMsgUnix(data, control)
	if err != nil {
		return -1, nil, err
	}
	messages, err := unix.ParseSocketControlMessage(control[:oobn])
	if err != nil {
		return -1, nil, err
	}
	var fds []int
	for _, message := range messages {
		received, parseErr := unix.ParseUnixRights(&message)
		if parseErr != nil {
			err = errors.Join(err, parseErr)
		} else {
			fds = append(fds, received...)
		}
	}
	adopted := false
	defer func() {
		if !adopted {
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
		}
	}()
	if err != nil || flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 || len(fds) != 1 || n == 0 {
		return -1, nil, errors.Join(errors.New("invalid memory pager fd framing"), err)
	}
	decoder := json.NewDecoder(io.MultiReader(bytes.NewReader(data[:n]), io.LimitReader(conn, int64(limit-n))))
	decoder.DisallowUnknownFields()
	var regions []memoryRegion
	if err = decoder.Decode(&regions); err != nil {
		return -1, nil, err
	}
	buffered, err := io.ReadAll(decoder.Buffered())
	if err != nil || len(bytes.TrimSpace(buffered)) != 0 {
		return -1, nil, errors.New("trailing memory mapping data")
	}
	if err = validateMemoryRegions(regions, size); err != nil {
		return -1, nil, err
	}
	unix.CloseOnExec(fds[0])
	if err = unix.SetNonblock(fds[0], true); err != nil {
		return -1, nil, err
	}
	adopted = true
	return fds[0], regions, nil
}

func newMemoryPager(ctx context.Context, fd int, regions []memoryRegion, source vm.MemorySource, stopVMM context.CancelFunc) *memoryPager {
	lifetime, cancel := context.WithCancel(ctx)
	p := &memoryPager{started: time.Now(), ctx: lifetime, cancel: cancel, stopVMM: stopVMM, fd: fd, regions: regions, source: source, done: make(chan struct{}), faults: make(chan memoryFault, 64), chunks: make([]memoryChunkState, (source.Size()-1)/vm.MemoryReadBytes+1)}
	p.workers.Go(p.readFaults)
	for range 3 {
		p.workers.Go(p.serveFaults)
	}
	return p
}
func (p *memoryPager) failure() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
func (p *memoryPager) fail(err error) {
	if err == nil || p.ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	// Process cancellation bypasses the Computer barrier, including while a
	// kernel-origin fault blocks a Firecracker API request during capture/load.
	p.stopVMM()
	p.cancel()
}
func (p *memoryPager) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() { p.cancel(); p.workers.Wait(); p.closeErr = unix.Close(p.fd) })
	return p.closeErr
}
func (p *memoryPager) StartHydration() {
	if p == nil {
		return
	}
	p.background.Do(func() {
		p.workers.Go(func() {
			started := time.Now()
			defer close(p.done)
			defer func() {
				reportPhase(p.recordPhase, vm.Phase{Name: "restore_memory_hydration", StartedAt: started, DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), Memory: p.stats(), ErrorClass: vm.RuntimeErrorClass(p.failure())})
			}()
			for offset := int64(0); offset < p.source.Size(); {
				if p.ctx.Err() != nil {
					return
				}
				p.mu.Lock()
				loaded := p.chunks[offset/vm.MemoryReadBytes].loaded
				p.mu.Unlock()
				if loaded {
					offset += min(int64(vm.MemoryReadBytes), p.source.Size()-offset)
					continue
				}
				if err := p.yieldToDemand(); err != nil {
					return
				}
				began := time.Now()
				var consumeNanos int64
				next, err := p.source.ReadWindows(p.ctx, offset, func(at int64, data []byte) error {
					started := time.Now()
					if err := p.yieldToDemand(); err != nil {
						return err
					}
					err := p.populateBackgroundWindow(at, data)
					consumeNanos += int64(time.Since(started))
					return err
				})
				p.backgroundReadNanos.Add(max(0, int64(time.Since(began))-consumeNanos))
				offset = next
				if err == nil {
					continue
				}
				p.backgroundLoadErrors.Add(1)
				if errors.Is(err, vm.ErrInvalidCheckpointContent) || errors.Is(err, errMemoryPager) {
					p.fail(err)
					return
				}
				timer := time.NewTimer(time.Second)
				select {
				case <-p.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		})
	})
}
func (p *memoryPager) hydrationReady() (bool, error) {
	if p == nil {
		return true, nil
	}
	if err := p.failure(); err != nil {
		return false, err
	}
	if err := p.ctx.Err(); err != nil {
		return false, err
	}
	select {
	case <-p.done:
		return true, p.failure()
	default:
		return false, nil
	}
}

func (p *memoryPager) WaitHydration(ctx context.Context) (result error) {
	if p == nil {
		return nil
	}
	select {
	case <-p.done:
		if err := p.failure(); err != nil {
			return err
		}
		return p.ctx.Err()
	default:
	}
	started := time.Now()
	defer func() {
		reportPhase(p.recordPhase, vm.Phase{Name: "restore_memory_hydration_wait", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), Memory: p.stats(), ErrorClass: func() string {
			if errors.Is(result, context.DeadlineExceeded) {
				return "timeout"
			}
			return vm.RuntimeErrorClass(result)
		}()})
	}()
	select {
	case <-p.done:
		if err := p.failure(); err != nil {
			return err
		}
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}
		return nil
	case <-p.ctx.Done():
		if err := p.failure(); err != nil {
			return err
		}
		return p.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *memoryPager) readFaults() {
	var data [32 * 64]byte
	for p.ctx.Err() == nil {
		descriptors := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
		_, err := unix.Poll(descriptors, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			p.fail(fmt.Errorf("%w: poll: %v", errMemoryPager, err))
			return
		}
		if descriptors[0].Revents == 0 {
			continue
		}
		if descriptors[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			p.fail(fmt.Errorf("%w: descriptor closed", errMemoryPager))
			return
		}
		n, err := unix.Read(p.fd, data[:])
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 || n%32 != 0 {
			p.fail(fmt.Errorf("%w: invalid event read: %v", errMemoryPager, err))
			return
		}
		received := time.Now()
		deadline := received.Add(memoryFaultDeadline)
		for at := 0; at < n; at += 32 {
			event := data[at : at+32]
			flags := binary.LittleEndian.Uint64(event[8:16])
			address := binary.LittleEndian.Uint64(event[16:24])
			// WRITE is ordinary missing-page behavior. WP/MINOR and all non-pagefault
			// events require mechanisms this full-memory contract does not enable.
			if event[0] != 0x12 || flags&^uint64(1) != 0 {
				p.fail(fmt.Errorf("%w: unsupported event", errMemoryPager))
				return
			}
			offset, ok := p.offsetForAddress(address)
			if !ok {
				p.fail(fmt.Errorf("%w: fault outside registered memory", errMemoryPager))
				return
			}
			p.faultCount.Add(1)
			window := offset / vm.MemoryReadBytes
			page := (offset % vm.MemoryReadBytes) / memoryPageBytes
			p.mu.Lock()
			state := &p.chunks[window]
			mask := uint64(1) << uint(page%64)
			if state.observedDemandPages[page/64]&mask == 0 {
				state.observedDemandPages[page/64] |= mask
				p.observedDemandPages.Add(1)
			}
			if !state.observedDemand {
				state.observedDemand = true
				p.observedDemandWindows.Add(1)
			}
			p.mu.Unlock()
			timer := time.NewTimer(time.Until(deadline))
			select {
			case p.faults <- memoryFault{chunk: offset / vm.MemoryReadBytes, deadline: deadline, received: received}:
				timer.Stop()
			case <-p.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				p.fail(fmt.Errorf("%w: fault queue: %w", errMemoryPager, context.DeadlineExceeded))
				return
			}
		}
	}
}
func (p *memoryPager) offsetForAddress(address uint64) (int64, bool) {
	for _, region := range p.regions {
		if address >= region.Base && address-region.Base < region.Size {
			return int64(region.Offset + address - region.Base), true
		}
	}
	return 0, false
}
func (p *memoryPager) serveFaults() {
	buffer := make([]byte, vm.MemoryReadBytes)
	defer clear(buffer)
	for {
		select {
		case <-p.ctx.Done():
			return
		case fault := <-p.faults:
			p.faultQueueNanos.Add(int64(time.Since(fault.received)))
			started := time.Now()
			ctx, cancel := context.WithDeadline(p.ctx, fault.deadline)
			observation := vm.MemoryFaultObservation{Window: fault.chunk, ReceivedNs: int64(fault.received.Sub(p.started)), StartedNs: int64(started.Sub(p.started))}
			p.mu.Lock()
			if p.demandActive == 0 {
				p.demandDone = make(chan struct{})
			}
			p.demandActive++
			p.mu.Unlock()
			err := p.loadDemandWindow(ctx, fault.chunk, &observation, buffer)
			p.mu.Lock()
			p.demandActive--
			if p.demandActive == 0 {
				close(p.demandDone)
			}
			p.mu.Unlock()
			observation.FinishedNs = int64(time.Since(p.started))
			p.demandDurationBuckets[min(31, bits.Len64(uint64(max(0, observation.FinishedNs-observation.StartedNs)/1000)))].Add(1)
			observation.ErrorClass = vm.RuntimeErrorClass(err)
			if observation.FinishedNs-observation.StartedNs >= int64(time.Millisecond) {
				p.mu.Lock()
				if len(p.faultObservations) < 256 {
					p.faultObservations = append(p.faultObservations, observation)
				} else {
					// Keep the slowest bounded operations across readiness and
					// later work; an early fault burst must not consume the trace.
					shortest := 0
					for i := range p.faultObservations {
						if p.faultObservations[i].FinishedNs-p.faultObservations[i].StartedNs < p.faultObservations[shortest].FinishedNs-p.faultObservations[shortest].StartedNs {
							shortest = i
						}
					}
					if observation.FinishedNs-observation.StartedNs > p.faultObservations[shortest].FinishedNs-p.faultObservations[shortest].StartedNs {
						p.faultObservations[shortest] = observation
					}
					p.droppedFaultObservations++
				}
				p.mu.Unlock()
			}
			p.faultServiceNanos.Add(int64(time.Since(started)))
			cancel()
			if err != nil {
				p.fail(err)
				return
			}
		}
	}
}

// Yield between bounded background windows. Faults never wait on this gate;
// their own read and population must complete before background work resumes.
func (p *memoryPager) yieldToDemand() error {
	for {
		p.mu.Lock()
		active, done := p.demandActive, p.demandDone
		p.mu.Unlock()
		if active == 0 {
			return p.ctx.Err()
		}
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-done:
		}
	}
}

// Only an authenticated complete window may enter this method. The source
// retains its borrowed buffer until return, including while a demand owner runs.
func (p *memoryPager) populateBackgroundWindow(offset int64, data []byte) error {
	ordinal := offset / vm.MemoryReadBytes
	for {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		state := &p.chunks[ordinal]
		if state.loaded {
			p.mu.Unlock()
			p.backgroundDiscardedWindows.Add(1)
			return nil
		}
		if state.loading != nil {
			done := state.loading
			p.mu.Unlock()
			started := time.Now()
			select {
			case <-done:
			case <-p.ctx.Done():
				return p.ctx.Err()
			}
			p.backgroundDemandWaitNanos.Add(int64(time.Since(started)))
			// A failed demand owner releases its state before stopping the VM.
			// Recheck instead of treating in-flight work as completed hydration.
			continue
		}
		done := make(chan struct{})
		state.loading, state.background = done, true
		p.mu.Unlock()
		p.backgroundLoads.Add(1)
		started := time.Now()
		err := p.populate(p.ctx, data, offset)
		p.backgroundPopulateNanos.Add(int64(time.Since(started)))
		p.mu.Lock()
		state.loading = nil
		state.loaded = err == nil
		if err == nil {
			p.chunkCount.Add((int64(len(data))-1)/vm.MemoryChunkBytes + 1)
		}
		close(done)
		p.mu.Unlock()
		return err
	}
}

func (p *memoryPager) loadDemandWindow(ctx context.Context, ordinal int64, observation *vm.MemoryFaultObservation, buffer []byte) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		state := &p.chunks[ordinal]
		if state.loaded {
			p.mu.Unlock()
			return nil
		}
		if state.loading != nil {
			done := state.loading
			background := state.background
			if background {
				p.joinBackground.Add(1)
			} else {
				p.joinDemand.Add(1)
			}
			p.mu.Unlock()
			waitStarted := time.Now()
			var waitErr error
			select {
			case <-done:
			case <-ctx.Done():
				waitErr = ctx.Err()
			}
			elapsed := int64(time.Since(waitStarted))
			p.joinDurationBuckets[min(31, bits.Len64(uint64(max(0, elapsed)/1000)))].Add(1)
			if background {
				p.joinBackgroundNanos.Add(elapsed)
				observation.JoinBackgroundNs += elapsed
			} else {
				p.joinDemandNanos.Add(elapsed)
				observation.JoinDemandNs += elapsed
			}
			if waitErr != nil {
				return waitErr
			}
			continue
		}
		done := make(chan struct{})
		state.loading = done
		state.background = false
		p.demandLoads.Add(1)
		p.mu.Unlock()
		offset := ordinal * vm.MemoryReadBytes
		buffer = buffer[:min(int64(vm.MemoryReadBytes), p.source.Size()-offset)]
		readStarted := time.Now()
		n, err := p.source.ReadAt(ctx, buffer, offset)
		readElapsed := int64(time.Since(readStarted))
		p.demandReadNanos.Add(readElapsed)
		observation.SourceReadNs += readElapsed
		if err == nil && n != len(buffer) {
			err = fmt.Errorf("%w: incomplete authenticated memory read", vm.ErrInvalidCheckpointContent)
		}
		if err == nil {
			copyStarted := time.Now()
			err = p.populate(ctx, buffer, offset)
			copyElapsed := int64(time.Since(copyStarted))
			p.demandPopulateNanos.Add(copyElapsed)
			observation.PopulateNs += copyElapsed
		}
		clear(buffer)
		if err != nil {
			p.demandLoadErrors.Add(1)
		}
		p.mu.Lock()
		state.loading = nil
		state.loaded = err == nil
		if err == nil {
			p.chunkCount.Add((int64(len(buffer))-1)/vm.MemoryChunkBytes + 1)
		}
		close(done)
		p.mu.Unlock()
		return err
	}
}
func (p *memoryPager) populate(ctx context.Context, data []byte, offset int64) error {
	for at := 0; at < len(data); at += memoryPageBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		logical := uint64(offset) + uint64(at)
		var destination uint64
		for _, region := range p.regions {
			if logical >= region.Offset && logical-region.Offset < region.Size {
				destination = region.Base + logical - region.Offset
				break
			}
		}
		if destination == 0 {
			return fmt.Errorf("%w: missing destination mapping", errMemoryPager)
		}
		if err := copyMissingMemoryPage(p.fd, destination, data[at:at+memoryPageBytes]); err != nil {
			if errors.Is(err, unix.ESRCH) {
				// The target address space has exited; no guest remains to hydrate.
				p.cancel()
				return context.Canceled
			}
			return fmt.Errorf("%w: populate page: %w", errMemoryPager, err)
		}
	}
	return nil
}
func copyMissingMemoryPage(fd int, destination uint64, data []byte) error {
	request := struct {
		Destination, Source, Length, Mode uint64
		Copied                            int64
	}{Destination: destination, Source: uint64(uintptr(unsafe.Pointer(&data[0]))), Length: memoryPageBytes}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(0xc028aa03), uintptr(unsafe.Pointer(&request)))
	runtime.KeepAlive(data)
	if errno == unix.EEXIST {
		return nil
	} // Never replace a guest-written resident page.
	if errno != 0 {
		return errno
	}
	if request.Copied != memoryPageBytes {
		return errors.New("incomplete UFFD page copy")
	}
	return nil
}

func listenMemoryPager(path string, uid, gid int) (*net.UnixListener, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err = errors.Join(os.Chown(path, uid, gid), os.Chmod(path, 0600)); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func (p *memoryPager) stats() *vm.MemoryRestoreStats {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	observations := append([]vm.MemoryFaultObservation(nil), p.faultObservations...)
	dropped := p.droppedFaultObservations
	p.mu.Unlock()
	stats := &vm.MemoryRestoreStats{
		ObservationStartedAt: p.started, ObservedDemandPages: p.observedDemandPages.Load(), ObservedDemandWindows: p.observedDemandWindows.Load(),
		DemandFaults: p.faultCount.Load(), LoadedChunks: p.chunkCount.Load(), TotalChunks: (p.source.Size()-1)/vm.MemoryChunkBytes + 1,
		DemandLoads: p.demandLoads.Load(), BackgroundLoads: p.backgroundLoads.Load(), BackgroundDiscardedWindows: p.backgroundDiscardedWindows.Load(), BackgroundDemandWaitMs: p.backgroundDemandWaitNanos.Load() / int64(time.Millisecond), DemandLoadErrors: p.demandLoadErrors.Load(), BackgroundLoadErrors: p.backgroundLoadErrors.Load(),
		JoinBackground: p.joinBackground.Load(), JoinDemand: p.joinDemand.Load(), JoinBackgroundMs: p.joinBackgroundNanos.Load() / int64(time.Millisecond), JoinDemandMs: p.joinDemandNanos.Load() / int64(time.Millisecond),
		FaultQueueMs: p.faultQueueNanos.Load() / int64(time.Millisecond), FaultServiceMs: p.faultServiceNanos.Load() / int64(time.Millisecond),
		DemandSourceReadMs: p.demandReadNanos.Load() / int64(time.Millisecond), BackgroundSourceReadMs: p.backgroundReadNanos.Load() / int64(time.Millisecond), DemandPopulateMs: p.demandPopulateNanos.Load() / int64(time.Millisecond), BackgroundPopulateMs: p.backgroundPopulateNanos.Load() / int64(time.Millisecond),
		FaultObservations: observations, DroppedFaultObservations: dropped,
	}
	for i := range p.demandDurationBuckets {
		stats.DemandDurationBuckets[i] = p.demandDurationBuckets[i].Load()
		stats.JoinDurationBuckets[i] = p.joinDurationBuckets[i].Load()
	}
	stats.MemoryReadStats = p.source.MemoryReadStats()
	return stats
}
