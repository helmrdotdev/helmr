//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

type pagerTestSource struct {
	data  []byte
	reads atomic.Int64
}

func (s *pagerTestSource) Size() int64 { return int64(len(s.data)) }
func (s *pagerTestSource) ReadAt(ctx context.Context, dst []byte, offset int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.reads.Add(1)
	return copy(dst, s.data[offset:]), nil
}

func TestMemoryRegionValidation(t *testing.T) {
	valid := []memoryRegion{{Base: 0x100000, Size: 4096, Offset: 0, PageSize: 4096}, {Base: 0x200000, Size: 4096, Offset: 4096, PageSize: 4096}}
	if err := validateMemoryRegions(valid, 8192); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]memoryRegion){
		func(r []memoryRegion) { r[1].Base = r[0].Base },
		func(r []memoryRegion) { r[1].Offset = 8192 },
		func(r []memoryRegion) { r[0].PageSize = 2 << 20 },
		func(r []memoryRegion) { r[0].Base++ },
		func(r []memoryRegion) { r[1].Size = 8192 },
	} {
		r := append([]memoryRegion(nil), valid...)
		change(r)
		if validateMemoryRegions(r, 8192) == nil {
			t.Fatalf("accepted invalid mapping: %+v", r)
		}
	}
}

func TestMemoryHandshakeDoesNotWaitForEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	file, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw, _ := json.Marshal([]memoryRegion{{Base: 0x100000, Size: 4096, PageSize: 4096}})
	if _, _, err = peer.WriteMsgUnix(raw, unix.UnixRights(int(file.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	fd, regions, err := receiveMemoryHandshake(ctx, listener, os.Getpid(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if len(regions) != 1 {
		t.Fatal(regions)
	}
}

func TestMemoryHandshakeRejectsForeignPeer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	fd, _, err := receiveMemoryHandshake(t.Context(), listener, os.Getpid()+1, 4096)
	if err == nil || fd != -1 {
		t.Fatalf("accepted foreign peer: fd=%d err=%v", fd, err)
	}
}

func testUserfaultMemory(t *testing.T, size int, kernelFaults bool) (int, []byte) {
	t.Helper()
	flags := unix.O_CLOEXEC | unix.O_NONBLOCK
	if !kernelFaults {
		flags |= 1
	}
	raw, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(flags), 0, 0)
	if errno != 0 {
		if os.Getenv("HELMR_REQUIRE_UFFD") == "1" {
			t.Fatal(errno)
		}
		t.Skipf("userfaultfd unavailable: %v", errno)
	}
	fd := int(raw)
	// These nonparallel fixtures fault inside the pager's Go process. A GC
	// stop-the-world would wait for that fault while blocking its resolution.
	// Production faults originate in the separate VMM process. Disable both GC
	// triggers until the mapping is gone, and leave a P free for the pager.
	limit := debug.SetMemoryLimit(math.MaxInt64)
	gc := debug.SetGCPercent(-1)
	procs := runtime.GOMAXPROCS(0)
	runtime.GOMAXPROCS(max(procs, 2))
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
		debug.SetMemoryLimit(limit)
		debug.SetGCPercent(gc)
	})
	t.Cleanup(func() { _ = unix.Close(fd) })
	api := struct{ API, Features, Ioctls uint64 }{API: 0xaa}
	if _, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0xc018aa3f, uintptr(unsafe.Pointer(&api))); errno != 0 {
		t.Fatal(errno)
	}
	memory, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Munmap(memory) })
	registration := struct{ Start, Length, Mode, Ioctls uint64 }{Start: uint64(uintptr(unsafe.Pointer(&memory[0]))), Length: uint64(size), Mode: 1}
	if _, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0xc020aa00, uintptr(unsafe.Pointer(&registration))); errno != 0 {
		t.Fatal(errno)
	}
	return fd, memory
}

func TestMemoryPagerDemandAndHydrationPreserveGuestWrites(t *testing.T) {
	fd, memory := testUserfaultMemory(t, 2*vm.MemoryReadBytes, false)
	// The pager owns a duplicate; the test retains its original until unmapping.
	owned, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	source := &pagerTestSource{data: bytes.Repeat([]byte{37}, len(memory))}
	stopped := atomic.Bool{}
	pager := newMemoryPager(t.Context(), owned, []memoryRegion{{Base: uint64(uintptr(unsafe.Pointer(&memory[0]))), Size: uint64(len(memory)), PageSize: 4096}}, source, func() { stopped.Store(true) })
	defer pager.Close()
	observed := make(chan byte, 1)
	go func() { observed <- memory[0] }()
	select {
	case got := <-observed:
		if got != 37 {
			t.Fatal(got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demand fault did not resolve")
	}
	// The adjacent MiB is populated by the same demand read.
	if got := memory[1<<20]; got != 37 || source.reads.Load() != 1 {
		t.Fatalf("adjacent byte=%d reads=%d", got, source.reads.Load())
	}
	memory[0] = 99
	pager.StartHydration()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = pager.WaitHydration(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pager.populate(ctx, source.data[:4096], 0); err != nil {
		t.Fatal(err)
	}
	// A fault wakes the guest before its demand owner marks the window loaded.
	// Background may read that window meanwhile, but must discard its copy.
	if memory[0] != 99 || memory[len(memory)-1] != 37 || stopped.Load() || source.reads.Load() != 2+pager.backgroundDiscardedWindows.Load() {
		t.Fatalf("write=%d tail=%d stopped=%v reads=%d", memory[0], memory[len(memory)-1], stopped.Load(), source.reads.Load())
	}
}

func TestEagerMemoryCancellationRemovesPartialFile(t *testing.T) {
	source := &pagerTestSource{data: bytes.Repeat([]byte{1}, 4096)}
	path := filepath.Join(t.TempDir(), "memory")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := materializeEagerMemory(ctx, path, source); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed restore retained memory", err)
	}
}

func TestMemoryPagerResolvesKernelOriginFault(t *testing.T) {
	fd, memory := testUserfaultMemory(t, vm.MemoryChunkBytes, true)
	owned, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	source := &pagerTestSource{data: bytes.Repeat([]byte{73}, len(memory))}
	pager := newMemoryPager(t.Context(), owned, []memoryRegion{{Base: uint64(uintptr(unsafe.Pointer(&memory[0]))), Size: uint64(len(memory)), PageSize: 4096}}, source, func() {})
	defer pager.Close()
	file, err := os.CreateTemp(t.TempDir(), "kernel-copy")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	completed := make(chan error, 1)
	go func() { _, err := unix.Write(int(file.Fd()), memory[:4096]); completed <- err }()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kernel-origin fault did not resolve")
	}
	got := make([]byte, 4096)
	if _, err = file.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, source.data[:4096]) || source.reads.Load() != 1 {
		t.Fatal("kernel copy differs from authenticated source")
	}
}

type stalledPagerSource struct{}

func (stalledPagerSource) Size() int64 { return vm.MemoryChunkBytes }
func (stalledPagerSource) ReadAt(ctx context.Context, _ []byte, _ int64) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func TestMemoryPagerDemandDeadlineStopsOwnedProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan struct{})
	p := &memoryPager{started: time.Now(), ctx: ctx, cancel: cancel, stopVMM: func() { close(stopped) }, source: stalledPagerSource{}, chunks: make([]memoryChunkState, 1), faults: make(chan memoryFault, 1)}
	exited := make(chan struct{})
	go func() { defer close(exited); p.serveFaults() }()
	p.faults <- memoryFault{chunk: 0, deadline: time.Now().Add(20 * time.Millisecond)}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("fault deadline did not stop process")
	}
	<-exited
	if !errors.Is(p.failure(), context.DeadlineExceeded) {
		t.Fatal(p.failure())
	}
}

type interruptedPagerSource struct {
	pagerTestSource
	interrupted atomic.Bool
}

func (s *interruptedPagerSource) ReadAt(ctx context.Context, dst []byte, offset int64) (int, error) {
	if !s.interrupted.Swap(true) {
		return 0, context.DeadlineExceeded
	}
	return s.pagerTestSource.ReadAt(ctx, dst, offset)
}
func TestMemoryPagerHydrationRetriesWithoutStoppingGuest(t *testing.T) {
	fd, memory := testUserfaultMemory(t, vm.MemoryChunkBytes, false)
	owned, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	source := &interruptedPagerSource{pagerTestSource: pagerTestSource{data: bytes.Repeat([]byte{49}, len(memory))}}
	stopped := atomic.Bool{}
	pager := newMemoryPager(t.Context(), owned, []memoryRegion{{Base: uint64(uintptr(unsafe.Pointer(&memory[0]))), Size: uint64(len(memory)), PageSize: 4096}}, source, func() { stopped.Store(true) })
	defer pager.Close()
	pager.StartHydration()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err = pager.WaitHydration(ctx); err != nil {
		t.Fatal(err)
	}
	if stopped.Load() || memory[0] != 49 || !source.interrupted.Load() {
		t.Fatal("background transient did not recover")
	}
}

func TestMemoryPagerQueueBackpressurePreservesFault(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[0])
	defer unix.Close(pair[1])
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &memoryPager{started: time.Now(), chunks: make([]memoryChunkState, 1), ctx: ctx, cancel: cancel, fd: pair[0], stopVMM: func() {}, faults: make(chan memoryFault, 64), regions: []memoryRegion{{Base: 4096, Size: vm.MemoryChunkBytes}}}
	for range cap(p.faults) {
		p.faults <- memoryFault{chunk: -1}
	}
	var event [32]byte
	event[0] = 0x12
	binary.LittleEndian.PutUint64(event[16:24], 4096)
	if _, err = unix.Write(pair[1], event[:]); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); p.readFaults() }()
	deadline := time.Now().Add(time.Second)
	for p.faultCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.faultCount.Load() != 1 {
		t.Fatal("event was not read")
	}
	if err := p.failure(); err != nil {
		t.Fatal("queue saturation stopped the pager", err)
	}
	<-p.faults
	for range 63 {
		<-p.faults
	}
	select {
	case fault := <-p.faults:
		if fault.chunk != 0 || fault.deadline.IsZero() {
			t.Fatal("pending fault lost its original deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("pending fault was dropped")
	}
	cancel()
	<-done
	if err := p.failure(); err != nil {
		t.Fatal(err)
	}
}

func (*pagerTestSource) MemoryReadStats() vm.MemoryReadStats   { return vm.MemoryReadStats{} }
func (stalledPagerSource) MemoryReadStats() vm.MemoryReadStats { return vm.MemoryReadStats{} }

// The opt-in dense case exercises a substantially resident mapping without
// charging every unit-test invocation for its 512 MiB fixture working set.
func TestMemoryPagerDenseWorkingSet(t *testing.T) {
	if os.Getenv("HELMR_MEMORY_HEAVY_PROOF") != "1" {
		t.Skip("set HELMR_MEMORY_HEAVY_PROOF=1 for the dense kernel-fault case")
	}
	// A userfaultfd trap blocks its Go P outside the syscall handoff. Reserve
	// spare Ps for the in-process pager while all eight readers may be faulting.
	previous := runtime.GOMAXPROCS(0)
	runtime.GOMAXPROCS(max(previous, 16))
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	const size = 256 << 20
	fd, memory := testUserfaultMemory(t, size, false)
	owned, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	source := &pagerTestSource{data: make([]byte, size)}
	if _, err := rand.Read(source.data); err != nil {
		t.Fatal(err)
	}
	stopped := atomic.Bool{}
	pager := newMemoryPager(t.Context(), owned, []memoryRegion{{Base: uint64(uintptr(unsafe.Pointer(&memory[0]))), Size: size, PageSize: 4096}}, source, func() { stopped.Store(true) })
	defer pager.Close()
	started := time.Now()
	var readers sync.WaitGroup
	for worker := range 8 {
		readers.Go(func() {
			for page := worker; page < size/4096; page += 8 {
				memory[page*4096] ^= 0xff
			}
		})
	}
	readers.Wait()
	pager.StartHydration()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := pager.WaitHydration(ctx); err != nil {
		t.Fatal(err)
	}
	for i, value := range memory {
		want := source.data[i]
		if i%4096 == 0 {
			want ^= 0xff
		}
		if value != want {
			t.Fatalf("memory differs at %d: %d != %d", i, value, want)
		}
	}
	// Every extra read must be an authenticated background window discarded
	// after joining a demand owner; guest writes above must remain intact.
	if stopped.Load() || source.reads.Load() != size/vm.MemoryReadBytes+pager.backgroundDiscardedWindows.Load() {
		t.Fatalf("stopped=%v source reads=%d", stopped.Load(), source.reads.Load())
	}
	t.Logf("dense bytes=%d elapsed=%s source_reads=%d pager=%+v", size, time.Since(started), source.reads.Load(), pager.stats())
}

func TestMemoryDemandJoinObservationIncludesDeadline(t *testing.T) {
	for _, background := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			pager := &memoryPager{chunks: []memoryChunkState{{loading: make(chan struct{}), background: background}}}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			observation := vm.MemoryFaultObservation{}
			if err := pager.loadDemandWindow(ctx, 0, &observation, make([]byte, vm.MemoryReadBytes)); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("join error: %v", err)
			}
			if background {
				if pager.joinBackground.Load() != 1 || pager.joinDemand.Load() != 0 || observation.JoinBackgroundNs != int64(10*time.Millisecond) || observation.JoinDemandNs != 0 {
					t.Fatalf("background join: %+v", observation)
				}
			} else {
				if pager.joinDemand.Load() != 1 || pager.joinBackground.Load() != 0 || observation.JoinDemandNs != int64(10*time.Millisecond) || observation.JoinBackgroundNs != 0 {
					t.Fatalf("demand join: %+v", observation)
				}
			}
		})
	}
}

func readTestMemoryWindow(ctx context.Context, source vm.MemorySource, offset int64, consume func(int64, []byte) error) (int64, error) {
	data := make([]byte, min(int64(vm.MemoryReadBytes), source.Size()-offset))
	defer clear(data)
	n, err := source.ReadAt(ctx, data, offset)
	if err != nil {
		return offset, err
	}
	if n != len(data) {
		return offset, vm.ErrInvalidCheckpointContent
	}
	if err := consume(offset, data); err != nil {
		return offset, err
	}
	return offset + int64(len(data)), nil
}
func (s *pagerTestSource) ReadWindows(ctx context.Context, offset int64, consume func(int64, []byte) error) (int64, error) {
	return readTestMemoryWindow(ctx, s, offset, consume)
}
func (s stalledPagerSource) ReadWindows(ctx context.Context, offset int64, consume func(int64, []byte) error) (int64, error) {
	return readTestMemoryWindow(ctx, s, offset, consume)
}
func (s *interruptedPagerSource) ReadWindows(ctx context.Context, offset int64, consume func(int64, []byte) error) (int64, error) {
	return readTestMemoryWindow(ctx, s, offset, consume)
}

func TestMemoryBackgroundWaitsForDemandBeforeAdvancing(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				demandDone := make(chan struct{})
				p := &memoryPager{ctx: ctx, fd: -1, chunks: []memoryChunkState{{loading: demandDone}}}
				finished := make(chan error, 1)
				go func() { finished <- p.populateBackgroundWindow(0, make([]byte, 4096)) }()
				synctest.Wait()
				select {
				case err := <-finished:
					t.Fatalf("advanced while demand owns window: %v", err)
				default:
				}
				p.mu.Lock()
				p.chunks[0].loaded = success
				p.chunks[0].loading = nil
				close(demandDone)
				p.mu.Unlock()
				err := <-finished
				if success {
					if err != nil || p.backgroundDiscardedWindows.Load() != 1 || p.backgroundLoads.Load() != 0 {
						t.Fatalf("did not discard after demand success: %v", err)
					}
				} else {
					if !errors.Is(err, errMemoryPager) || p.chunks[0].loaded || p.backgroundLoads.Load() != 1 {
						t.Fatalf("did not reclaim failed demand window: %v", err)
					}
				}
			})
		})
	}
}

func TestMemoryBackgroundDemandWaitCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		p := &memoryPager{ctx: ctx, chunks: []memoryChunkState{{loading: done}}}
		finished := make(chan error, 1)
		go func() { finished <- p.populateBackgroundWindow(0, make([]byte, 4096)) }()
		synctest.Wait()
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if p.chunks[0].loading != done || p.chunks[0].loaded {
			t.Fatal("changed demand owner on cancellation")
		}
	})
}

type gatedStreamSource struct {
	pagerTestSource
	started chan struct{}
	release chan struct{}
}

func (s *gatedStreamSource) ReadWindows(ctx context.Context, offset int64, consume func(int64, []byte) error) (int64, error) {
	close(s.started)
	select {
	case <-s.release:
	case <-ctx.Done():
		return offset, ctx.Err()
	}
	return readTestMemoryWindow(ctx, &s.pagerTestSource, offset, consume)
}
func TestMemoryStreamingDoesNotReserveDemandWindow(t *testing.T) {
	fd, memory := testUserfaultMemory(t, vm.MemoryReadBytes, false)
	owned, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	s := &gatedStreamSource{pagerTestSource: pagerTestSource{data: bytes.Repeat([]byte{17}, len(memory))}, started: make(chan struct{}), release: make(chan struct{})}
	p := newMemoryPager(t.Context(), owned, []memoryRegion{{Base: uint64(uintptr(unsafe.Pointer(&memory[0]))), Size: uint64(len(memory)), PageSize: 4096}}, s, func() {})
	defer p.Close()
	p.StartHydration()
	<-s.started
	got := make(chan byte, 1)
	go func() { got <- memory[0] }()
	select {
	case value := <-got:
		if value != 17 {
			t.Fatal(value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demand waited for background network")
	}
	memory[0] = 99
	close(s.release)
	if err := p.WaitHydration(t.Context()); err != nil {
		t.Fatal(err)
	}
	if memory[0] != 99 || p.backgroundDiscardedWindows.Load() != 1 || p.joinBackground.Load() != 0 {
		t.Fatalf("guest write/ownership changed: %+v", p.stats())
	}
}

type partialStreamSource struct {
	pagerTestSource
	calls   []int64
	failure error
}

func (s *partialStreamSource) ReadWindows(ctx context.Context, offset int64, consume func(int64, []byte) error) (int64, error) {
	s.calls = append(s.calls, offset)
	stop := s.Size()
	if len(s.calls) == 1 {
		stop = 2 * vm.MemoryReadBytes
	}
	for offset < stop {
		next, err := readTestMemoryWindow(ctx, &s.pagerTestSource, offset, consume)
		if err != nil {
			return next, err
		}
		offset = next
	}
	if len(s.calls) == 1 {
		return offset, s.failure
	}
	return offset, nil
}
func TestMemoryStreamingHydrationRetainsPartialProgress(t *testing.T) {
	for _, integrity := range []bool{false, true} {
		t.Run(fmt.Sprint(integrity), func(t *testing.T) {
			fd, memory := testUserfaultMemory(t, 4*vm.MemoryReadBytes, false)
			owned, err := unix.Dup(fd)
			if err != nil {
				t.Fatal(err)
			}
			failure := error(context.DeadlineExceeded)
			if integrity {
				failure = vm.ErrInvalidCheckpointContent
			}
			s := &partialStreamSource{pagerTestSource: pagerTestSource{data: bytes.Repeat([]byte{54}, len(memory))}, failure: failure}
			stopped := atomic.Bool{}
			p := newMemoryPager(t.Context(), owned, []memoryRegion{{Base: uint64(uintptr(unsafe.Pointer(&memory[0]))), Size: uint64(len(memory)), PageSize: 4096}}, s, func() { stopped.Store(true) })
			defer p.Close()
			p.StartHydration()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err = p.WaitHydration(ctx)
			if integrity {
				if !errors.Is(err, vm.ErrInvalidCheckpointContent) || !stopped.Load() || len(s.calls) != 1 || p.chunkCount.Load() != 2*vm.MemoryReadBytes/vm.MemoryChunkBytes {
					t.Fatalf("integrity failure not terminal: %v %+v", err, p.stats())
				}
			} else {
				if err != nil || stopped.Load() || len(s.calls) != 2 || s.calls[1] != 2*vm.MemoryReadBytes || p.chunkCount.Load() != 4*vm.MemoryReadBytes/vm.MemoryChunkBytes || p.backgroundLoads.Load() != 4 || p.backgroundLoadErrors.Load() != 1 {
					t.Fatalf("lost/repeated progress: %v calls=%v %+v", err, s.calls, p.stats())
				}
				if !bytes.Equal(memory, s.data) {
					t.Fatal("restored memory differs")
				}
			}
		})
	}
}
