package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
)

const (
	networkPolicyTableName        = "helmr_network_policy"
	runNetworkDeniedCounterName   = "run_denied"
	buildNetworkDeniedCounterName = "build_denied"
	buildNetworkLimitCounterName  = "build_limit"
)

func renderRunNetworkPolicy(
	policy compute.NetworkPolicy,
	blockedIPv4CIDRs []string,
	blockedIPv6CIDRs []string,
) string {
	chainPolicy := "accept"
	terminalDeny := ""
	if !policy.Internet {
		chainPolicy = "drop"
		terminalDeny = fmt.Sprintf(
			"add rule inet %s forward counter name %s drop",
			networkPolicyTableName,
			runNetworkDeniedCounterName,
		)
	}
	return fmt.Sprintf(strings.TrimSpace(`
add table inet %[1]s
add counter inet %[1]s %[2]s
add chain inet %[1]s forward { type filter hook forward priority 0; policy %[3]s; }
add rule inet %[1]s forward meta nfproto ipv6 counter name %[2]s drop
add rule inet %[1]s forward ct state established,related accept
%[4]s
%[5]s
add rule inet %[1]s forward ip daddr @blocked_ipv4 counter name %[2]s drop
add rule inet %[1]s forward ip6 daddr @blocked_ipv6 counter name %[2]s drop
%[6]s
	`)+"\n",
		networkPolicyTableName,
		runNetworkDeniedCounterName,
		chainPolicy,
		runNetworkPolicySet("blocked_ipv4", "ipv4_addr", blockedIPv4CIDRs),
		runNetworkPolicySet("blocked_ipv6", "ipv6_addr", blockedIPv6CIDRs),
		terminalDeny,
	)
}

func parseRunNetworkStatus(raw []byte) (vm.RunNetworkStatus, error) {
	counters, err := parseNetworkCounters(raw, "Run")
	if err != nil {
		return vm.RunNetworkStatus{}, err
	}
	packets, ok := counters[runNetworkDeniedCounterName]
	if !ok {
		return vm.RunNetworkStatus{}, errors.New("Run denied counter is missing")
	}
	return vm.RunNetworkStatus{DeniedPackets: packets}, nil
}

func parseBuildNetworkStatus(raw []byte) (vm.BuildNetworkStatus, error) {
	counters, err := parseNetworkCounters(raw, "build")
	if err != nil {
		return vm.BuildNetworkStatus{}, err
	}
	denied, foundDenied := counters[buildNetworkDeniedCounterName]
	limit, foundLimit := counters[buildNetworkLimitCounterName]
	if !foundDenied || !foundLimit {
		return vm.BuildNetworkStatus{}, errors.New(
			"build network counters are incomplete",
		)
	}
	return vm.BuildNetworkStatus{
		DeniedPackets: denied,
		LimitPackets:  limit,
	}, nil
}

func parseNetworkCounters(raw []byte, label string) (map[string]uint64, error) {
	var document struct {
		Objects []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
			} `json:"counter,omitempty"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf(
			"decode %s network counters: %w",
			label,
			err,
		)
	}
	counters := make(map[string]uint64)
	for _, object := range document.Objects {
		if object.Counter == nil {
			continue
		}
		if _, exists := counters[object.Counter.Name]; exists {
			return nil, fmt.Errorf(
				"%s network counter %q is duplicated",
				label,
				object.Counter.Name,
			)
		}
		counters[object.Counter.Name] = object.Counter.Packets
	}
	return counters, nil
}

func runNetworkPolicySet(name string, nftType string, cidrs []string) string {
	if len(cidrs) == 0 {
		return fmt.Sprintf(
			"add set inet %s %s { type %s; flags interval; }",
			networkPolicyTableName,
			name,
			nftType,
		)
	}
	return fmt.Sprintf(
		"add set inet %s %s { type %s; flags interval; elements = { %s } }",
		networkPolicyTableName,
		name,
		nftType,
		strings.Join(cidrs, ", "),
	)
}
