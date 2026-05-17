//go:build linux

package firecracker

import (
	"context"
	"strings"
	"testing"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
)

func TestNFTNetworkPolicyScriptBlocksConfiguredCIDRs(t *testing.T) {
	script := nftNetworkPolicyScript(
		[]string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"},
		[]string{"fc00::/7", "fe80::/10"},
	)
	for _, want := range []string{
		"add table inet helmr_network_policy",
		"type filter hook forward priority 0; policy accept;",
		"udp dport 53 accept",
		"tcp dport 53 accept",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"fc00::/7",
		"fe80::/10",
		"ip daddr @blocked_ipv4 drop",
		"ip6 daddr @blocked_ipv6 drop",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	assertRuleBefore(t, script, "ip daddr @blocked_ipv4 drop", "udp dport 53 accept")
	assertRuleBefore(t, script, "ip daddr @blocked_ipv4 drop", "tcp dport 53 accept")
	assertRuleBefore(t, script, "ip6 daddr @blocked_ipv6 drop", "udp dport 53 accept")
	assertRuleBefore(t, script, "ip6 daddr @blocked_ipv6 drop", "tcp dport 53 accept")
}

func TestNFTNetworkPolicyScriptUsesConfiguredCIDRs(t *testing.T) {
	script := nftNetworkPolicyScript([]string{"198.18.0.0/15"}, []string{"2001:db8::/32"})
	for _, want := range []string{"198.18.0.0/15", "2001:db8::/32"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing configured CIDR %q:\n%s", want, script)
		}
	}
	for _, blockedDefault := range []string{"10.0.0.0/8", "fc00::/7"} {
		if strings.Contains(script, blockedDefault) {
			t.Fatalf("script unexpectedly contains default CIDR %q:\n%s", blockedDefault, script)
		}
	}
}

func TestWithNetworkPolicySurvivesSnapshotHandlerReplacement(t *testing.T) {
	connector := &Connector{cfg: (Config{}).WithDefaults()}
	machine, err := fc.NewMachine(
		context.Background(),
		fc.Config{},
		fc.WithSnapshot("/tmp/mem", "/tmp/state"),
		connector.withNetworkPolicy("vm-1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !machine.Handlers.FcInit.Has("fcinit.ApplyHelmrNetworkPolicy") {
		t.Fatal("network policy handler was not installed after snapshot handlers")
	}
}

func assertRuleBefore(t *testing.T, script, before, after string) {
	t.Helper()
	beforeIndex := strings.Index(script, before)
	if beforeIndex == -1 {
		t.Fatalf("script missing earlier rule %q:\n%s", before, script)
	}
	afterIndex := strings.Index(script, after)
	if afterIndex == -1 {
		t.Fatalf("script missing later rule %q:\n%s", after, script)
	}
	if beforeIndex > afterIndex {
		t.Fatalf("rule %q must appear before %q:\n%s", before, after, script)
	}
}
