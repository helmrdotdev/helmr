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
	loaded  bool
	loading chan struct{}
}
type memoryFault struct {
	chunk    int64
	deadline time.Time
}

// One pager owns one physical VMM's UFFD and its authenticated memory source.
// Three demand workers and one background reader bound memory I/O independently
// of disk. Background work never queues ahead of faults or owns their deadline.
type memoryPager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	stopVMM     context.CancelFunc
	fd          int
	source      vm.MemorySource
	regions     []memoryRegion
	workers     sync.WaitGroup
	background  sync.Once
	done        chan struct{}
	faults      chan memoryFault
	mu          sync.Mutex
	chunks      []memoryChunkState
	err         error
	closeOnce   sync.Once
	closeErr    error
	recordPhase func(vm.Phase)
	faultCount  atomic.Int64
	chunkCount  atomic.Int64
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
	p := &memoryPager{ctx: lifetime, cancel: cancel, stopVMM: stopVMM, fd: fd, regions: regions, source: source, done: make(chan struct{}), faults: make(chan memoryFault, 64), chunks: make([]memoryChunkState, (source.Size()-1)/vm.MemoryReadBytes+1)}
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
				reportPhase(p.recordPhase, vm.Phase{Name: "restore_memory_hydration", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), Memory: p.stats(), ErrorClass: vm.RuntimeErrorClass(p.failure())})
			}()
			for i := int64(0); i < int64(len(p.chunks)); {
				if p.ctx.Err() != nil {
					return
				}
				err := p.loadChunk(p.ctx, i)
				if err == nil {
					i++
					continue
				}
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
func (p *memoryPager) WaitHydration(ctx context.Context) error {
	if p == nil {
		return nil
	}
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
		deadline := time.Now().Add(memoryFaultDeadline)
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
			timer := time.NewTimer(time.Until(deadline))
			select {
			case p.faults <- memoryFault{chunk: offset / vm.MemoryReadBytes, deadline: deadline}:
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
	for {
		select {
		case <-p.ctx.Done():
			return
		case fault := <-p.faults:
			ctx, cancel := context.WithDeadline(p.ctx, fault.deadline)
			err := p.loadChunk(ctx, fault.chunk)
			cancel()
			if err != nil {
				p.fail(err)
				return
			}
		}
	}
}
func (p *memoryPager) loadChunk(ctx context.Context, ordinal int64) error {
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
			p.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		state.loading = done
		p.mu.Unlock()
		offset := ordinal * vm.MemoryReadBytes
		buffer := make([]byte, min(int64(vm.MemoryReadBytes), p.source.Size()-offset))
		n, err := p.source.ReadAt(ctx, buffer, offset)
		if err == nil && n != len(buffer) {
			err = fmt.Errorf("%w: incomplete authenticated memory read", vm.ErrInvalidCheckpointContent)
		}
		if err == nil {
			err = p.populate(ctx, buffer, offset)
		}
		clear(buffer)
		p.mu.Lock()
		state.loading = nil
		state.loaded = err == nil
		close(done)
		p.mu.Unlock()
		if err == nil {
			p.chunkCount.Add((int64(len(buffer))-1)/vm.MemoryChunkBytes + 1)
		}
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
	stats := &vm.MemoryRestoreStats{DemandFaults: p.faultCount.Load(), LoadedChunks: p.chunkCount.Load(), TotalChunks: (p.source.Size()-1)/vm.MemoryChunkBytes + 1}
	stats.MemoryReadStats = p.source.MemoryReadStats()
	return stats
}
