//go:build linux

package firecracker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/vishvananda/netns"
	"golang.org/x/net/dns/dnsmessage"
)

// Run on a disposable privileged Linux host. The guest helper runs as root in
// its own namespace, and never receives a proxy environment configuration.
// Synthetic dial failures prevent any fixture traffic from reaching the internet.
func TestNetworkGatewayNamespace(t *testing.T) {
	if mode := os.Getenv("HELMR_NETWORK_GATEWAY_CHILD"); mode != "" {
		networkGatewayGuest(t, mode)
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
	owner, guest := fmt.Sprintf("hnp-host-%d", os.Getpid()), fmt.Sprintf("hnp-guest-%d", os.Getpid())
	run("ip", "netns", "add", owner)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", owner).Run() })
	run("ip", "netns", "add", guest)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", guest).Run() })
	run("ip", "link", "add", "tap0", "netns", owner, "type", "veth", "peer", "name", "eth0", "netns", guest)
	for _, spec := range []struct{ ns, device, ip string }{{owner, "tap0", "192.168.127.1/30"}, {guest, "eth0", "192.168.127.2/30"}} {
		run("ip", "-n", spec.ns, "addr", "add", spec.ip, "dev", spec.device)
		run("ip", "-n", spec.ns, "link", "set", spec.device, "up")
		run("ip", "-n", spec.ns, "link", "set", "lo", "up")
	}
	run("ip", "-n", guest, "route", "add", "default", "via", "192.168.127.1")
	handle, err := netns.GetFromName(owner)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := withNetworkNamespace(handle, func() error { return os.WriteFile("/proc/sys/net/ipv4/conf/all/rp_filter", []byte("0\n"), 0644) }); err != nil {
		t.Fatal(err)
	}
	var listener net.Listener
	if err := withNetworkNamespace(handle, func() error {
		var e error
		listener, e = listenSecretEgress()
		if e != nil {
			return e
		}
		return installSecretRoute("tap0", 71)
	}); err != nil {
		t.Fatal(err)
	}
	policy := networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "api.example.com"}}}
	decisions := secretproxy.NewDecisionLog(slog.Default(), "", owner, "instance", owner, networkpolicy.PhaseRuntime, policy)
	var resolutions, dials atomic.Int32
	proxy, err := secretproxy.New(secretproxy.Config{NetworkPolicy: policy, AllowedDestination: func(netip.Addr) bool { return true }, LookupIP: func(ctx context.Context, network, name string) ([]netip.Addr, error) {
		resolutions.Add(1)
		if network != "ip4" || name != "api.example.com." {
			t.Errorf("unapproved resolver request %s %s", network, name)
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("8.8.8.8")}, nil
	}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if network != "tcp4" || address != "8.8.8.8:443" {
			t.Errorf("unapproved numeric dial %s %s", network, address)
		}
		return nil, errors.New("synthetic upstream")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if err := proxy.BindDecisionLog(decisions); err != nil {
		t.Fatal(err)
	}
	go func() { _ = proxy.Serve(listener) }()
	var udp net.PacketConn
	var dnsTCP net.Listener
	if err := withNetworkNamespace(handle, func() error {
		var e error
		udp, e = net.ListenPacket("udp4", "0.0.0.0:53")
		if e != nil {
			return e
		}
		dnsTCP, e = net.Listen("tcp4", "0.0.0.0:53")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	dns, err := secretproxy.NewDNS(policy, networkpolicy.Installation{}, func(ctx context.Context, network, name string) ([]netip.Addr, error) {
		resolutions.Add(1)
		if name != "api.example.com." || network != "ip4" {
			t.Errorf("unapproved DNS lookup %s %s", network, name)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")}, nil
	}, udp, dnsTCP, decisions)
	if err != nil {
		t.Fatal(err)
	}
	defer dns.Close()
	dns.Serve()
	script, err := renderNetworkPolicy(networkPolicyInput{Policy: policy, Tap: "tap0", Peer: "host0", Mark: 71, GuestIPv4: "192.168.127.2", TranslationIPv4: "100.96.0.2", ResolverIPv4: "10.0.0.2", BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}})
	if err != nil {
		t.Fatal(err)
	}
	script += "add table inet fixture_mark\nadd chain inet fixture_mark pre { type filter hook prerouting priority -160; policy accept; }\nadd rule inet fixture_mark pre iifname \"tap0\" meta mark set 71\n"
	cmd := exec.Command("ip", "netns", "exec", owner, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install restrictive boundary: %v %s", err, raw)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"allowed", "allowed_dns", "denied_dns", "aaaa_dns", "denied_sni", "no_sni", "plaintext", "http80", "quic", "alternate_dns", "ipv6", "metadata"} {
		beforeDNS, beforeDial := resolutions.Load(), dials.Load()
		cmd := exec.Command("ip", "netns", "exec", guest, executable, "-test.run=^TestNetworkGatewayNamespace$", "-test.timeout=5s")
		cmd.Env = append(os.Environ(), "HELMR_NETWORK_GATEWAY_CHILD="+mode)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("guest %s: %v %s", mode, err, raw)
		}
		if mode == "allowed" {
			if resolutions.Load() != beforeDNS+1 || dials.Load() != beforeDial+1 {
				t.Fatal("allowed SNI did not reach exact trusted numeric dial")
			}
		} else if mode == "allowed_dns" {
			if resolutions.Load() != beforeDNS+1 || dials.Load() != beforeDial {
				t.Fatal("DNS did not use selective resolver")
			}
		} else if resolutions.Load() != beforeDNS || dials.Load() != beforeDial {
			t.Fatalf("%s caused upstream contact", mode)
		}
	}
	blockedScript, err := renderNetworkPolicy(networkPolicyInput{Policy: networkpolicy.BlockedPolicy(), Tap: "tap0", Peer: "host0", Mark: 71, GuestIPv4: "192.168.127.2", TranslationIPv4: "100.96.0.2", ResolverIPv4: "10.0.0.2", BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}})
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("ip", "netns", "exec", owner, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader("delete table inet " + networkPolicyTableName + "\n" + blockedScript)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install fixed seed boundary: %v %s", err, raw)
	}
	beforeDNS, beforeDial := resolutions.Load(), dials.Load()
	for _, mode := range []string{"blocked_tls", "blocked_dns"} {
		cmd := exec.Command("ip", "netns", "exec", guest, executable, "-test.run=^TestNetworkGatewayNamespace$", "-test.timeout=5s")
		cmd.Env = append(os.Environ(), "HELMR_NETWORK_GATEWAY_CHILD="+mode)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("full-block guest %s: %v %s", mode, err, raw)
		}
	}
	if resolutions.Load() != beforeDNS || dials.Load() != beforeDial {
		t.Fatal("full-block phase leaked through still-running services")
	}

}
func networkGatewayGuest(t *testing.T, mode string) {
	if mode == "allowed_dns" || mode == "denied_dns" || mode == "aaaa_dns" || mode == "blocked_dns" {
		conn, err := net.DialTimeout("udp4", "10.0.0.2:53", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		host, kind := "api.example.com.", dnsmessage.TypeA
		if mode == "denied_dns" {
			host = "denied.example.com."
		}
		if mode == "aaaa_dns" {
			kind = dnsmessage.TypeAAAA
		}
		name, _ := dnsmessage.NewName(host)
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7})
		_ = builder.StartQuestions()
		_ = builder.Question(dnsmessage.Question{Name: name, Type: kind, Class: dnsmessage.ClassINET})
		packet, _ := builder.Finish()
		if _, err := conn.Write(packet); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, 4096)
		n, err := conn.Read(reply)
		if mode == "blocked_dns" {
			if err == nil {
				t.Fatal("full-block DNS returned a packet")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		var result dnsmessage.Message
		if err := result.Unpack(reply[:n]); err != nil {
			t.Fatal(err)
		}
		if mode == "allowed_dns" {
			if len(result.Answers) != 1 || result.Header.RCode != 0 {
				t.Fatal("allowed DNS failed")
			}
		} else if mode == "denied_dns" {
			if result.Header.RCode != dnsmessage.RCodeRefused {
				t.Fatal("unlisted DNS wasn't refused")
			}
		} else if len(result.Answers) != 0 || result.Header.RCode != 0 {
			t.Fatal("matching AAAA wasn't NODATA")
		}
		return
	}
	target, network := "93.184.216.34:443", "tcp4"
	switch mode {
	case "http80":
		target = "93.184.216.34:80"
	case "quic":
		network = "udp4"
	case "alternate_dns":
		network = "udp4"
		target = "8.8.4.4:53"
	case "ipv6":
		network = "tcp6"
		target = "[2001:4860:4860::8888]:443"
	case "metadata":
		target = "169.254.169.254:443"
	}
	conn, err := net.DialTimeout(network, target, 300*time.Millisecond)
	if err != nil {
		if mode == "allowed" {
			t.Fatal(err)
		}
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if mode == "allowed" || mode == "denied_sni" || mode == "no_sni" {
		host := "api.example.com"
		if mode == "denied_sni" {
			host = "denied.example.com"
		}
		if mode == "no_sni" {
			host = ""
		}
		client := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true})
		_ = client.Handshake()
		return
	}
	_, _ = conn.Write([]byte("synthetic probe"))
	var reply [1]byte
	if n, _ := conn.Read(reply[:]); n != 0 {
		t.Fatal("denied protocol returned data")
	}
}
