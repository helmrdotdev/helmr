package firecracker

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
)

func TestRunNetworkPolicyContractCountsEveryDenyPath(t *testing.T) {
	script := renderRunNetworkPolicy(
		compute.NetworkPolicy{Internet: false},
		[]string{"169.254.0.0/16"},
	)
	for _, want := range []string{
		"add counter inet helmr_network_policy run_denied",
		"meta nfproto ipv6 counter name run_denied drop",
		"ip daddr @blocked_ipv4 counter name run_denied drop",
		"forward counter name run_denied drop",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "blocked_ipv6") {
		t.Fatalf("script contains an unreachable IPv6 set:\n%s", script)
	}
	ipv6Drop := strings.Index(
		script,
		"meta nfproto ipv6 counter name run_denied drop",
	)
	establishedAccept := strings.Index(
		script,
		"ct state established,related accept",
	)
	if ipv6Drop < 0 ||
		establishedAccept < 0 ||
		ipv6Drop > establishedAccept {
		t.Fatalf("IPv6 deny must precede established accept:\n%s", script)
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
