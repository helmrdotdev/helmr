package firecracker

import (
	"net/netip"
	"strings"
	"testing"
)

func renderNetworkPolicyForTest(t *testing.T) string {
	t.Helper()
	script, err := renderNetworkPolicy(networkPolicyInput{
		Tap: "tap0", Peer: "veth0", Mark: 71,
		GuestIPv4: "169.254.64.2", TranslationIPv4: "100.96.0.2",
		BlockedIPv4CIDRs: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.0/30"),
			netip.MustParsePrefix("192.0.2.4/32"),
		},
		ResolverIPv4: "10.20.0.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func TestNetworkPolicyUsesSuppliedDenySetAndDNSException(t *testing.T) {
	script := renderNetworkPolicyForTest(t)
	for _, prefix := range []string{"192.0.2.0/30", "192.0.2.4/32"} {
		if !strings.Contains(script, prefix) {
			t.Fatalf("script is missing supplied prefix %q:\n%s", prefix, script)
		}
	}
	dns := strings.Index(script, "ip daddr 10.20.0.2 udp dport 53 jump egress")
	deny := strings.Index(script, "ip daddr @blocked_ipv4 counter name run_denied drop")
	publicTCP := strings.Index(script, "meta l4proto tcp jump egress")
	if dns < 0 || deny < 0 || publicTCP < 0 || !(dns < deny && deny < publicTCP) {
		t.Fatalf("DNS exception, deny set, and public allow rules are out of order:\n%s", script)
	}
}

func TestRunNetworkPolicyIsClosedAroundBinding(t *testing.T) {
	script := renderNetworkPolicyForTest(t)
	for _, want := range []string{
		"policy drop",
		"meta nfproto ipv6 counter name run_denied drop",
		"ct state invalid counter name run_denied drop",
		`iifname "tap0" oifname "veth0" meta mark != 71`,
		`iifname "veth0" oifname "tap0" ip daddr 169.254.64.2 ct state established,related accept`,
		"meta l4proto tcp jump egress",
		"meta l4proto udp jump egress",
		"ip protocol icmp jump egress",
		"ct state established,related accept",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script is missing %q:\n%s", want, script)
		}
	}
	ipv6Drop := strings.Index(script, "meta nfproto ipv6 counter name run_denied drop")
	returnAccept := strings.Index(script, "ct state established,related accept")
	if ipv6Drop < 0 || returnAccept < 0 || ipv6Drop > returnAccept {
		t.Fatalf("IPv6 deny must precede established return acceptance:\n%s", script)
	}
}

func TestNetworkPolicyRendererRejectsIncompleteBinding(t *testing.T) {
	valid := networkPolicyInput{
		Tap: "tap0", Peer: "veth0", Mark: 1,
		GuestIPv4: "169.254.64.2", TranslationIPv4: "100.96.0.2",
		BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		ResolverIPv4:     "1.1.1.1",
	}
	tests := map[string]networkPolicyInput{
		"zero mark":      func() networkPolicyInput { input := valid; input.Mark = 0; return input }(),
		"same interface": func() networkPolicyInput { input := valid; input.Peer = input.Tap; return input }(),
		"empty deny set": func() networkPolicyInput { input := valid; input.BlockedIPv4CIDRs = nil; return input }(),
		"non-IPv4 deny": func() networkPolicyInput {
			input := valid
			input.BlockedIPv4CIDRs = []netip.Prefix{netip.MustParsePrefix("fc00::/7")}
			return input
		}(),
		"missing resolver": func() networkPolicyInput {
			input := valid
			input.ResolverIPv4 = ""
			return input
		}(),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := renderNetworkPolicy(input); err == nil {
				t.Fatal("invalid network policy input was accepted")
			}
		})
	}
}

func TestRunNetworkCounterContractRejectsMissingAndDuplicate(t *testing.T) {
	status, err := parseRunNetworkStatus([]byte(`{
		"nftables":[
			{"counter":{"name":"run_denied","packets":7}}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if status.DeniedPackets != 7 {
		t.Fatalf("Run network status = %+v", status)
	}
	for _, raw := range []string{
		`{"nftables":[]}`,
		`{"nftables":[
			{"counter":{"name":"run_denied","packets":1}},
			{"counter":{"name":"run_denied","packets":2}}
		]}`,
	} {
		if _, err := parseRunNetworkStatus([]byte(raw)); err == nil {
			t.Fatalf("invalid Run counters were accepted: %s", raw)
		}
	}
}
