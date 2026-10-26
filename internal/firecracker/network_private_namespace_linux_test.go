//go:build linux

package firecracker

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/vishvananda/netns"
)

func TestNetworkOrdinaryPrivateNamespace(t *testing.T) {
	if target := os.Getenv("HELMR_NETWORK_PRIVATE_CHILD"); target != "" {
		conn, err := net.DialTimeout("udp4", target, 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		_, _ = conn.Write([]byte("fixture"))
		var reply [16]byte
		n, err := conn.Read(reply[:])
		expect := os.Getenv("HELMR_NETWORK_PRIVATE_EXPECT") == "yes"
		if expect && (err != nil || string(reply[:n]) != "fixture") {
			t.Fatalf("expected private/public echo: %v", err)
		}
		if !expect && n > 0 {
			t.Fatal("private destination bypassed installation ceiling")
		}
		return
	}
	if os.Getenv("HELMR_TEST_NAMESPACE") != "1" {
		t.Skip("requires disposable privileged Linux namespace fixture")
	}
	run := func(args ...string) {
		t.Helper()
		if raw, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, raw)
		}
	}
	owner, guest, exit := fmt.Sprintf("hnc-host-%d", os.Getpid()), fmt.Sprintf("hnc-guest-%d", os.Getpid()), fmt.Sprintf("hnc-exit-%d", os.Getpid())
	for _, name := range []string{owner, guest, exit} {
		run("ip", "netns", "add", name)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", name).Run() })
		run("ip", "-n", name, "link", "set", "lo", "up")
	}
	run("ip", "link", "add", "tap0", "netns", owner, "type", "veth", "peer", "name", "eth0", "netns", guest)
	run("ip", "link", "add", "host0", "netns", owner, "type", "veth", "peer", "name", "eth0", "netns", exit)
	for _, spec := range []struct{ ns, device, ip string }{{owner, "tap0", "192.168.127.1/30"}, {guest, "eth0", "192.168.127.2/30"}, {owner, "host0", "100.96.0.2/24"}, {exit, "eth0", "100.96.0.1/24"}, {exit, "lo", "10.60.1.1/32"}, {exit, "lo", "8.8.8.8/32"}, {exit, "lo", "169.254.169.254/32"}} {
		run("ip", "-n", spec.ns, "addr", "add", spec.ip, "dev", spec.device)
		run("ip", "-n", spec.ns, "link", "set", spec.device, "up")
	}
	run("ip", "-n", guest, "route", "add", "default", "via", "192.168.127.1")
	run("ip", "-n", owner, "route", "add", "default", "via", "100.96.0.1")
	handle, err := netns.GetFromName(owner)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := withNetworkNamespace(handle, func() error {
		if e := os.WriteFile("/proc/sys/net/ipv4/conf/all/rp_filter", []byte("0\n"), 0644); e != nil {
			return e
		}
		return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
	}); err != nil {
		t.Fatal(err)
	}
	external, err := netns.GetFromName(exit)
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	for _, address := range []string{"8.8.8.8:7777", "10.60.1.1:7777", "169.254.169.254:7777"} {
		var server net.PacketConn
		if err := withNetworkNamespace(external, func() error { var e error; server, e = net.ListenPacket("udp4", address); return e }); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		t.Cleanup(func() { server.Close(); <-done })
		go func() {
			defer close(done)
			buffer := make([]byte, 512)
			for {
				n, peer, err := server.ReadFrom(buffer)
				if err != nil {
					return
				}
				_, _ = server.WriteTo(buffer[:n], peer)
			}
		}()
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for index, mode := range []string{"ordinary_empty", "ordinary_private", "block"} {
		policy := networkpolicy.OrdinaryPolicy()
		var ceiling []netip.Prefix
		if mode == "ordinary_private" {
			ceiling = []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}
		}
		if mode == "block" {
			policy = networkpolicy.BlockedPolicy()
		}
		script, err := renderNetworkPolicy(networkPolicyInput{Policy: policy, PrivateBusinessCIDRs: ceiling, Tap: "tap0", Peer: "host0", Mark: 71, GuestIPv4: "192.168.127.2", TranslationIPv4: "100.96.0.2", ResolverIPv4: "1.1.1.1", BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("10.90.0.0/16")}})
		if err != nil {
			t.Fatal(err)
		}
		if index > 0 {
			script = "delete table inet " + networkPolicyTableName + "\n" + script
		} else {
			script += "add table inet fixture_mark\nadd chain inet fixture_mark pre { type filter hook prerouting priority -160; policy accept; }\nadd rule inet fixture_mark pre iifname \"tap0\" meta mark set 71\n"
		}
		cmd := exec.Command("ip", "netns", "exec", owner, "nft", "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install %s: %v %s", mode, err, raw)
		}
		for _, target := range []string{"8.8.8.8:7777", "10.60.1.1:7777", "169.254.169.254:7777"} {
			expect := "no"
			if mode != "block" && (strings.HasPrefix(target, "8.") || mode == "ordinary_private" && strings.HasPrefix(target, "10.")) {
				expect = "yes"
			}
			cmd := exec.Command("ip", "netns", "exec", guest, executable, "-test.run=^TestNetworkOrdinaryPrivateNamespace$", "-test.timeout=5s")
			cmd.Env = append(os.Environ(), "HELMR_NETWORK_PRIVATE_CHILD="+target, "HELMR_NETWORK_PRIVATE_EXPECT="+expect)
			if raw, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s %s expected %s: %v %s", mode, target, expect, err, raw)
			}
		}
	}
}
