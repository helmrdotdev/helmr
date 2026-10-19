//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
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
	if memory[0] != 99 || memory[len(memory)-1] != 37 || stopped.Load() || source.reads.Load() != 2 {
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
	p := &memoryPager{ctx: ctx, cancel: cancel, stopVMM: func() { close(stopped) }, source: stalledPagerSource{}, chunks: make([]memoryChunkState, 1), faults: make(chan memoryFault, 1)}
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
	p := &memoryPager{ctx: ctx, cancel: cancel, fd: pair[0], stopVMM: func() {}, faults: make(chan memoryFault, 64), regions: []memoryRegion{{Base: 4096, Size: vm.MemoryChunkBytes}}}
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
	defer runtime.GOMAXPROCS(previous)
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
	if stopped.Load() || source.reads.Load() != size/vm.MemoryReadBytes {
		t.Fatalf("stopped=%v source reads=%d", stopped.Load(), source.reads.Load())
	}
	t.Logf("dense bytes=%d elapsed=%s source_reads=%d pager=%+v", size, time.Since(started), source.reads.Load(), pager.stats())
}
