//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"
)

// Keep the process main thread occupied by the test runner in this disposable
// fixture. Go permanently parks a locked main thread on goroutine exit rather
// than removing its /proc task, so the disappearance assertion below must use
// a non-main worker thread.
func init() {
	if os.Getenv("HELMR_TEST_NAMESPACE") == "1" {
		runtime.LockOSThread()
	}
}

func TestSecretProxyNamespaceSocketAndFailedRestoreRetirement(t *testing.T) {
	if os.Getenv("HELMR_TEST_NAMESPACE") != "1" {
		t.Skip("requires disposable Linux namespace capability")
	}
	runtime.LockOSThread()
	current, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	target, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := netns.Set(current); err != nil {
		t.Fatal(err)
	}
	runtime.UnlockOSThread()
	var listener net.Listener
	if err := withNetworkNamespace(target, func() error {
		link, e := netlink.LinkByName("lo")
		if e != nil {
			return e
		}
		if e := netlink.LinkSetUp(link); e != nil {
			return e
		}
		listener, e = net.Listen("tcp4", "127.0.0.1:0")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan error, 1)
	go func() {
		conn, e := listener.Accept()
		if e == nil {
			defer conn.Close()
			var value [1]byte
			_, e = conn.Read(value[:])
			if value[0] != 42 {
				e = errors.New("unexpected namespace payload")
			}
		}
		received <- e
	}()
	if err := withNetworkNamespace(target, func() error {
		conn, e := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
		if e != nil {
			return e
		}
		defer conn.Close()
		_, e = conn.Write([]byte{42})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	// Inject the exact restore-error branch while the worker is in a distinct
	// namespace. The contaminated kernel thread must disappear, never be pooled.
	var tid int
	err = withNetworkNamespaceRestore(target, func() error { tid = unix.Gettid(); return nil }, func(netns.NsHandle) error { return errors.New("synthetic restore failure") })
	if err == nil {
		t.Fatal("restore error hidden")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, e := os.Stat(fmt.Sprintf("/proc/self/task/%d", tid)); os.IsNotExist(e) {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("failed-restore OS thread was not retired")
}

func TestSecretProxyPreparationRequiredExceptQualification(t *testing.T) {
	c := &Connector{}
	if _, err := c.prepareSecretTransport(t.Context(), "assigned-runtime", nil); err == nil {
		t.Fatal("missing real-runtime preparation accepted")
	}
	calls := 0
	expected := errors.New("synthetic preparation failure")
	blocked := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	c.cfg.PrepareSecretTransport = func(ctx context.Context, id string, prefixes []netip.Prefix) (*secretproxy.Proxy, error) {
		calls++
		if id != "assigned-runtime" || !slices.Equal(prefixes, blocked) {
			t.Fatal("preparation lost runtime or policy")
		}
		return nil, expected
	}
	if _, err := c.prepareSecretTransport(context.WithValue(t.Context(), startupProbeNetworkKey{}, true), "synthetic-probe", blocked); err != nil || calls != 0 {
		t.Fatal("qualification reached control plane")
	}
	if _, err := c.prepareSecretTransport(t.Context(), "assigned-runtime", blocked); !errors.Is(err, expected) || calls != 1 {
		t.Fatal("real preparation failure did not stop network setup")
	}
}
