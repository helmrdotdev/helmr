//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

type processMemorySource struct{ size int64 }

func (s processMemorySource) Size() int64                       { return s.size }
func (processMemorySource) MemoryReadStats() vm.MemoryReadStats { return vm.MemoryReadStats{} }
func (s processMemorySource) ReadAt(ctx context.Context, dst []byte, offset int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for i := range dst {
		dst[i] = byte((offset + int64(i)) / 4096)
	}
	return len(dst), nil
}
func (s processMemorySource) ReadWindows(ctx context.Context, offset int64, consume func(int64, []byte) error) (int64, error) {
	buffer := make([]byte, vm.MemoryReadBytes)
	for offset < s.size {
		window := buffer[:min(int64(len(buffer)), s.size-offset)]
		if _, err := s.ReadAt(ctx, window, offset); err != nil {
			return offset, err
		}
		if err := consume(offset, window); err != nil {
			return offset, err
		}
		offset += int64(len(window))
	}
	return offset, nil
}

// Only the process standing in for the VMM disables GC. The owning pager runs
// in the parent with ordinary GC, just as it does with Firecracker.
func TestMemoryPagerProcessGuest(t *testing.T) {
	socket := os.Getenv("HELMR_TEST_PAGER_SOCKET")
	if socket == "" {
		t.Skip("subprocess fixture")
	}
	fd, memory := testUserfaultMemory(t, 64<<20, false)
	peer, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	regions := []memoryRegion{{Base: uint64(uintptr(unsafe.Pointer(&memory[0]))), Size: uint64(len(memory)), PageSize: 4096}}
	raw, err := json.Marshal(regions)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := peer.WriteMsgUnix(raw, unix.UnixRights(fd), nil); err != nil {
		t.Fatal(err)
	}
	peer.Close()
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for worker := range 4 {
		readers.Go(func() {
			for page := worker; page < len(memory)/4096; page += 4 {
				index := page * 4096
				if memory[index] != byte(page) {
					t.Errorf("page %d: got %d", page, memory[index])
					return
				}
				memory[index] ^= 0xff
			}
		})
	}
	readers.Wait()
	for page := range len(memory) / 4096 {
		if memory[page*4096] != byte(page)^0xff {
			t.Fatalf("guest write lost at page %d", page)
		}
	}
}

func TestMemoryPagerConcurrentProcessesWithGC(t *testing.T) {
	raw, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|1), 0, 0)
	if errno != 0 {
		if os.Getenv("HELMR_REQUIRE_UFFD") == "1" {
			t.Fatal(errno)
		}
		t.Skipf("userfaultfd unavailable: %v", errno)
	}
	unix.Close(int(raw))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type guest struct {
		cmd    *exec.Cmd
		output *bytes.Buffer
		start  io.WriteCloser
	}
	var guests []guest
	for range 4 {
		socket := filepath.Join(t.TempDir(), "pager.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMemoryPagerProcessGuest$", "-test.v")
		cmd.Env = append(os.Environ(), "HELMR_TEST_PAGER_SOCKET="+socket, "HELMR_REQUIRE_UFFD=1")
		output := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = output, output
		start, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		fd, regions, err := receiveMemoryHandshake(ctx, listener, cmd.Process.Pid, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		pager := newMemoryPager(ctx, fd, regions, processMemorySource{64 << 20}, func() { _ = cmd.Process.Kill() })
		t.Cleanup(func() { pager.Close() })
		guests = append(guests, guest{cmd, output, start})
	}
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stopGC := make(chan struct{})
	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		for {
			select {
			case <-stopGC:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stopGC); <-gcDone }()
	for _, guest := range guests {
		if _, err := guest.start.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		guest.start.Close()
	}
	for _, guest := range guests {
		if err := guest.cmd.Wait(); err != nil {
			t.Fatalf("guest: %v\n%s", err, guest.output)
		}
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.NumGC <= before.NumGC {
		t.Fatal("pager workload completed without a GC")
	}
	t.Logf("four independent guests, 256 MiB demanded, %d collections; heap alloc=%d heap system=%d", after.NumGC-before.NumGC, after.HeapAlloc, after.HeapSys)
}
