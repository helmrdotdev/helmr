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
	for _, unexpected := range []string{
		"udp dport 53 accept",
		"tcp dport 53 accept",
	} {
		if strings.Contains(script, unexpected) {
			t.Fatalf("script unexpectedly contains broad DNS exception %q:\n%s", unexpected, script)
		}
	}
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
