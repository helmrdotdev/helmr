//go:build linux

package firecracker

import (
	"context"
	"strings"
	"testing"

	"github.com/firecracker-microvm/firecracker-go-sdk"
)

func TestNFTBuildNetworkPolicyScriptClosesProgramBuildEgress(t *testing.T) {
	ipv4, err := effectiveBuildBlockedCIDRs(
		[]string{"54.240.0.0/16"},
	)
	if err != nil {
		t.Fatal(err)
	}
	script, err := nftBuildNetworkPolicyScript(
		"tap0",
		[]string{"1.1.1.1", "2606:4700:4700::1111"},
		ipv4,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"policy drop",
		"ct state new ct count over 256 counter name build_limit drop",
		"quota name build_sent counter name build_limit drop",
		"quota name build_received counter name build_limit drop",
		`iifname "tap0" ip daddr @resolver_ipv4 udp dport 53 jump egress`,
		`iifname "tap0" ip daddr @resolver_ipv4 tcp dport 53 jump egress`,
		`iifname "tap0" ip daddr @blocked_ipv4 counter name build_denied drop`,
		"meta nfproto ipv6 counter name build_denied drop",
		`iifname "tap0" tcp jump egress`,
		"counter name build_denied drop",
		"198.18.0.0/15",
		"203.0.113.0/24",
		"54.240.0.0/16",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("build policy script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "udp accept") {
		t.Fatalf("build policy contains a broad UDP allowance:\n%s", script)
	}
	for _, unreachable := range []string{"blocked_ipv6", "resolver_ipv6"} {
		if strings.Contains(script, unreachable) {
			t.Fatalf("build policy contains unreachable %q state:\n%s", unreachable, script)
		}
	}
}

func TestBuildNetworkPolicyRendererAcceptsExactManagedCloudSet(t *testing.T) {
	script, err := nftBuildNetworkPolicyScript(
		"tap0",
		[]string{"10.0.0.2"},
		managedCloudDenyPrefixStrings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantElements := "elements = { " +
		strings.Join(managedCloudDenyPrefixStrings(), ", ") +
		" }"
	if strings.Count(script, wantElements) != 1 {
		t.Fatalf("script does not contain exact managed-Cloud set %q:\n%s", wantElements, script)
	}
	for _, legacy := range []string{
		"192.0.0.0/24",
		"192.0.2.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
	} {
		if strings.Contains(script, legacy) {
			t.Fatalf("script contains legacy broad-catalog prefix %q:\n%s", legacy, script)
		}
	}
}

func TestParseBuildNetworkStatusRequiresBothCounters(t *testing.T) {
	status, err := parseBuildNetworkStatus([]byte(`{
		"nftables": [
			{"metainfo": {"json_schema_version": 1}},
			{"counter": {"family": "inet", "name": "build_denied", "table": "helmr_network_policy", "packets": 2, "bytes": 120}},
			{"counter": {"family": "inet", "name": "build_limit", "table": "helmr_network_policy", "packets": 3, "bytes": 180}}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if status.DeniedPackets != 2 || status.LimitPackets != 3 {
		t.Fatalf("build network status = %+v", status)
	}
	if _, err := parseBuildNetworkStatus([]byte(`{
		"nftables": [
			{"counter": {"name": "build_denied", "packets": 1}}
		]
	}`)); err == nil {
		t.Fatal("incomplete build network counters were accepted")
	}
}

func TestNFTNetworkPolicyScriptBlocksConfiguredCIDRs(t *testing.T) {
	script := renderRunNetworkPolicy(
		[]string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"},
	)
	for _, want := range []string{
		"add table inet helmr_network_policy",
		"add counter inet helmr_network_policy run_denied",
		"type filter hook forward priority 0; policy accept;",
		"meta nfproto ipv6 counter name run_denied drop",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"ip daddr @blocked_ipv4 counter name run_denied drop",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "blocked_ipv6") {
		t.Fatalf("script contains an unreachable IPv6 set:\n%s", script)
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
	script := renderRunNetworkPolicy([]string{"198.18.0.0/15"})
	for _, want := range []string{"198.18.0.0/15"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing configured CIDR %q:\n%s", want, script)
		}
	}
	for _, blockedDefault := range []string{"10.0.0.0/8"} {
		if strings.Contains(script, blockedDefault) {
			t.Fatalf("script unexpectedly contains default CIDR %q:\n%s", blockedDefault, script)
		}
	}
}

func TestNFTNetworkPolicyScriptDropsIPv6BeforeEstablishedTraffic(t *testing.T) {
	script := renderRunNetworkPolicy(nil)
	ipv6Drop := strings.Index(script, "meta nfproto ipv6 counter name run_denied drop")
	established := strings.Index(script, "ct state established,related accept")
	if ipv6Drop < 0 || established < 0 || ipv6Drop > established {
		t.Fatalf("IPv6 must be dropped before established traffic is accepted:\n%s", script)
	}
}

func TestParseRunNetworkStatusRequiresOneDeniedCounter(t *testing.T) {
	status, err := parseRunNetworkStatus([]byte(`{
		"nftables": [
			{"metainfo": {"json_schema_version": 1}},
			{"counter": {"family": "inet", "name": "run_denied", "table": "helmr_network_policy", "packets": 7, "bytes": 420}}
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

func TestWithNetworkPolicySurvivesSnapshotHandlerReplacement(t *testing.T) {
	connector := &Connector{cfg: (Config{}).WithDefaults()}
	machine, err := firecracker.NewMachine(
		context.Background(),
		firecracker.Config{},
		firecracker.WithSnapshot("/tmp/mem", "/tmp/state"),
		connector.withNetworkPolicy("vm-1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !machine.Handlers.FcInit.Has("fcinit.ApplyHelmrNetworkPolicy") {
		t.Fatal("network policy handler was not installed after snapshot handlers")
	}
}
