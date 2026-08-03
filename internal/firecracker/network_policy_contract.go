package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/vm"
)

const (
	networkPolicyTableName        = "helmr_network_policy"
	runNetworkDeniedCounterName   = "run_denied"
	buildNetworkDeniedCounterName = "build_denied"
	buildNetworkLimitCounterName  = "build_limit"
	buildNetworkReceivedQuotaName = "build_received"
	buildNetworkSentQuotaName     = "build_sent"
	buildNetworkConnectionLimit   = 256
	buildNetworkReceivedLimitMiB  = 10 * 1024
	buildNetworkSentLimitMiB      = 1024
)

type networkPolicyInput struct {
	Tap              string
	Peer             string
	Mark             uint32
	GuestIPv4        string
	TranslationIPv4  string
	BlockedIPv4CIDRs []netip.Prefix
	ResolverIPv4     string
	Build            bool
}

func renderNetworkPolicy(input networkPolicyInput) (string, error) {
	if input.Mark == 0 {
		return "", errors.New("network policy binding is invalid")
	}
	guestIPv4, err := netip.ParseAddr(strings.TrimSpace(input.GuestIPv4))
	if err != nil || !guestIPv4.Is4() || guestIPv4.IsUnspecified() {
		return "", errors.New("network policy guest IPv4 is invalid")
	}
	translationIPv4, err := netip.ParseAddr(strings.TrimSpace(input.TranslationIPv4))
	if err != nil || !translationIPv4.Is4() || translationIPv4.IsUnspecified() {
		return "", errors.New("network policy translation IPv4 is invalid")
	}
	for _, name := range []string{input.Tap, input.Peer} {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r\n") {
			return "", errors.New("network policy interface is invalid")
		}
	}
	if input.Tap == input.Peer {
		return "", errors.New("network policy interfaces must be distinct")
	}
	if len(input.BlockedIPv4CIDRs) == 0 {
		return "", errors.New("network policy blocked IPv4 set is empty")
	}
	blocked := make([]netip.Prefix, len(input.BlockedIPv4CIDRs))
	for index, prefix := range input.BlockedIPv4CIDRs {
		if !prefix.IsValid() || !prefix.Addr().Is4() || prefix != prefix.Masked() {
			return "", fmt.Errorf("network policy blocked prefix %q is not canonical IPv4", prefix)
		}
		blocked[index] = prefix
	}
	address, err := netip.ParseAddr(strings.TrimSpace(input.ResolverIPv4))
	if err != nil || !address.Is4() || address.IsUnspecified() {
		return "", errors.New("network policy resolver is not specified IPv4")
	}
	resolver := address.String()
	blockedCIDRs := canonicalIPv4PrefixSet(blocked)

	tap := strconv.Quote(input.Tap)
	peer := strconv.Quote(input.Peer)
	mark := strconv.FormatUint(uint64(input.Mark), 10)
	var script strings.Builder
	fmt.Fprintf(&script, "add table inet %s\n", networkPolicyTableName)
	if input.Build {
		fmt.Fprintf(&script, "add counter inet %s %s\n", networkPolicyTableName, buildNetworkDeniedCounterName)
		fmt.Fprintf(&script, "add counter inet %s %s\n", networkPolicyTableName, buildNetworkLimitCounterName)
		fmt.Fprintf(&script, "add quota inet %s %s { over %d mbytes }\n", networkPolicyTableName, buildNetworkReceivedQuotaName, buildNetworkReceivedLimitMiB)
		fmt.Fprintf(&script, "add quota inet %s %s { over %d mbytes }\n", networkPolicyTableName, buildNetworkSentQuotaName, buildNetworkSentLimitMiB)
	} else {
		fmt.Fprintf(&script, "add counter inet %s %s\n", networkPolicyTableName, runNetworkDeniedCounterName)
	}
	fmt.Fprintln(&script, runNetworkPolicySet("blocked_ipv4", "ipv4_addr", blockedCIDRs))
	fmt.Fprintf(&script, "add chain inet %s input { type filter hook input priority 0; policy drop; }\n", networkPolicyTableName)
	fmt.Fprintf(&script, "add chain inet %s output { type filter hook output priority 0; policy drop; }\n", networkPolicyTableName)
	fmt.Fprintf(&script, "add chain inet %s forward { type filter hook forward priority 0; policy drop; }\n", networkPolicyTableName)
	fmt.Fprintf(&script, "add chain inet %s egress\n", networkPolicyTableName)
	fmt.Fprintf(&script, "add chain inet %s postrouting { type nat hook postrouting priority srcnat; policy accept; }\n", networkPolicyTableName)
	deniedCounter := runNetworkDeniedCounterName
	if input.Build {
		deniedCounter = buildNetworkDeniedCounterName
	}
	fmt.Fprintf(&script, "add rule inet %s forward meta nfproto ipv6 counter name %s drop\n", networkPolicyTableName, deniedCounter)
	fmt.Fprintf(&script, "add rule inet %s forward ct state invalid counter name %s drop\n", networkPolicyTableName, deniedCounter)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s meta mark != %s counter name %s drop\n", networkPolicyTableName, tap, peer, mark, deniedCounter)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s meta mark %s ip saddr %s ip daddr %s udp dport 53 jump egress\n", networkPolicyTableName, tap, peer, mark, guestIPv4, resolver)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s meta mark %s ip saddr %s ip daddr %s tcp dport 53 jump egress\n", networkPolicyTableName, tap, peer, mark, guestIPv4, resolver)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s meta mark %s ip saddr %s ip daddr @blocked_ipv4 counter name %s drop\n", networkPolicyTableName, tap, peer, mark, guestIPv4, deniedCounter)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s meta mark %s ip saddr %s meta l4proto tcp jump egress\n", networkPolicyTableName, tap, peer, mark, guestIPv4)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s meta mark %s ip saddr %s meta l4proto udp jump egress\n", networkPolicyTableName, tap, peer, mark, guestIPv4)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s meta mark %s ip saddr %s ip protocol icmp jump egress\n", networkPolicyTableName, tap, peer, mark, guestIPv4)
	if input.Build {
		fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s quota name %s counter name %s drop\n", networkPolicyTableName, peer, tap, buildNetworkReceivedQuotaName, buildNetworkLimitCounterName)
	}
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname %s ip daddr %s ct state established,related accept\n", networkPolicyTableName, peer, tap, guestIPv4)
	fmt.Fprintf(&script, "add rule inet %s forward counter name %s drop\n", networkPolicyTableName, deniedCounter)
	if input.Build {
		fmt.Fprintf(&script, "add rule inet %s egress ct state new ct count over %d counter name %s drop\n", networkPolicyTableName, buildNetworkConnectionLimit, buildNetworkLimitCounterName)
		fmt.Fprintf(&script, "add rule inet %s egress quota name %s counter name %s drop\n", networkPolicyTableName, buildNetworkSentQuotaName, buildNetworkLimitCounterName)
	}
	fmt.Fprintf(&script, "add rule inet %s egress accept\n", networkPolicyTableName)
	fmt.Fprintf(&script, "add rule inet %s postrouting oifname %s ip saddr %s snat to %s\n", networkPolicyTableName, peer, guestIPv4, translationIPv4)
	return script.String(), nil
}

func canonicalIPv4PrefixSet(prefixes []netip.Prefix) []string {
	slices.SortFunc(prefixes, func(left, right netip.Prefix) int {
		if left.Bits() != right.Bits() {
			return left.Bits() - right.Bits()
		}
		return left.Addr().Compare(right.Addr())
	})
	result := make([]netip.Prefix, 0, len(prefixes))
	for _, candidate := range prefixes {
		covered := false
		for _, existing := range result {
			if existing.Contains(candidate.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, candidate)
		}
	}
	encoded := make([]string, len(result))
	for index, prefix := range result {
		encoded[index] = prefix.String()
	}
	return encoded
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
