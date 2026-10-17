//go:build linux

package nbd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This exercises the kernel queue, including buffered writeback and the helper's
// fsync. A fake ioctl cannot establish that terminal cleanup keeps O_EXCL custody.
// A failed run may require an operator clear on the dedicated device.
func TestTerminalTransportCompletesIOAndRetainsClaim(t *testing.T) {
	device := os.Getenv("HELMR_NBD_RECOVERY_DEVICE")
	if device == "" {
		t.Skip("requires an explicitly authorized idle dedicated Linux NBD device")
	}
	if err := VerifyIdleDevices([]string{device}); err != nil {
		t.Fatal(err)
	}
	arena, err := os.MkdirTemp("", "helmr-nbd-terminal-")
	if err != nil {
		t.Fatal(err)
	}
	// Retain the arena on any failure; it remains evidence for operator cleanup.
	t.Logf("owned terminal transport arena: %s", arena)
	listener, err := net.Listen("unix", filepath.Join(arena, "nbd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocked := &terminalWriteDevice{entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(blocked.release) })
	defer unblock()
	serverDone := make(chan error, 1)
	go func() { serverDone <- ServeOnce(ctx, listener, blocked, 8<<20) }()
	c := &claim{cfg: Config{Arena: arena, Socket: listener.Addr().String(), Devices: []string{device}, Size: 8 << 20}, configured: make(chan error, 1), running: make(chan error, 1)}
	c.boot, err = readText("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	c.start, err = readText("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	driverDone := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		c.running <- c.attach()
		close(driverDone)
	}()
	select {
	case err := <-c.configured:
		if err != nil {
			t.Fatal(err)
		}
	case <-driverDone:
		t.Fatalf("driver ended before configuration: %v", <-c.running)
	case <-time.After(5 * time.Second):
		t.Fatal("driver configuration did not finish")
	}
	for i := 0; ; i++ {
		if err := c.alive(); err == nil {
			break
		} else if i == 100 {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() {
		if t.Failed() && c.file != nil {
			// This test owns the dedicated device; unblock failed-test fsyncs
			// without closing its exclusive claim or hiding retained evidence.
			if err := ioctl(int(c.file.Fd()), clearSock, 0); err != nil {
				t.Logf("failed-test terminal clear: %v", err)
			}
		}
	}()
	consumer, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumer.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	consumerDone, helperDone := make(chan error, 1), make(chan error, 1)
	go func() { consumerDone <- consumer.Sync() }()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("kernel did not submit the buffered write")
	}
	go func() { helperDone <- c.file.Sync() }()
	// Supported live pressure must not complete the write or release its claim.
	select {
	case err := <-consumerDone:
		t.Fatalf("live write completed before storage responded: %v", err)
	case err := <-helperDone:
		t.Fatalf("live helper flush completed before storage responded: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := VerifyIdleDevices([]string{device}); !errors.Is(err, unix.EBUSY) {
		t.Fatalf("live claim was not exclusive: %v", err)
	}
	cancel() // End the actual transport, independently of the control loop.
	for name, done := range map[string]<-chan error{"consumer fsync": consumerDone, "helper fsync": helperDone} {
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s acknowledged a write the backend never completed", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s remained blocked after transport termination", name)
		}
	}
	select {
	case <-driverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal driver cleanup did not finish")
	}
	// The backend cannot reply until after the driver has cleared the dead
	// transport, so ordinary error replies cannot make this proof pass.
	unblock()
	<-serverDone
	if err := VerifyIdleDevices([]string{device}); !errors.Is(err, unix.EBUSY) {
		t.Fatalf("terminal cleanup released the exclusive claim: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.release(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyIdleDevices([]string{device}); err != nil {
		t.Fatal("device did not become idle after explicit release", err)
	}
	if err := os.RemoveAll(arena); err != nil {
		t.Fatal(err)
	}
}

type terminalWriteDevice struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *terminalWriteDevice) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	clear(p)
	return len(p), nil
}
func (d *terminalWriteDevice) WriteAt(ctx context.Context, _ []byte, _ int64) (int, error) {
	d.once.Do(func() { close(d.entered) })
	<-d.release
	return 0, ctx.Err()
}
func (d *terminalWriteDevice) Trim(ctx context.Context, _ int64, _ int) error {
	return ctx.Err()
}
func (d *terminalWriteDevice) Flush(ctx context.Context) error { return ctx.Err() }
