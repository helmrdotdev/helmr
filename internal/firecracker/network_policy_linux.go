//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
)

const (
	networkPolicyTableName        = "helmr_network_policy"
	buildNetworkDeniedCounterName = "build_denied"
	buildNetworkLimitCounterName  = "build_limit"
	buildNetworkReceivedQuotaName = "build_received"
	buildNetworkSentQuotaName     = "build_sent"
	buildNetworkConnectionLimit   = 256
	buildNetworkReceivedLimitMiB  = 10 * 1024
	buildNetworkSentLimitMiB      = 1024
)

var buildBlockedIPv4CIDRs = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.31.196.0/24",
	"192.52.193.0/24",
	"192.88.99.0/24",
	"192.168.0.0/16",
	"192.175.48.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
}

var buildBlockedIPv6CIDRs = []string{
	"::/128",
	"::1/128",
	"::ffff:0:0/96",
	"64:ff9b::/96",
	"64:ff9b:1::/48",
	"100::/64",
	"2001::/23",
	"2002::/16",
	"3fff::/20",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
}

func (c *Connector) withNetworkPolicy(netns string, policy compute.NetworkPolicy) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.SetupNetworkHandlerName, firecracker.Handler{
			Name: "fcinit.ApplyHelmrNetworkPolicy",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
				if c.kernelArgsValue() == buildInstallKernelArgs {
					tap, resolvers, err := buildNetworkInterface(machine)
					if err != nil {
						return err
					}
					return c.applyBuildNetworkPolicy(
						ctx,
						netns,
						policy,
						tap,
						resolvers,
					)
				}
				return c.applyNetworkPolicy(ctx, netns, policy)
			},
		})
	}
}

func (c *Connector) applyBuildNetworkPolicy(
	ctx context.Context,
	netns string,
	policy compute.NetworkPolicy,
	tap string,
	resolvers []string,
) error {
	blockedIPv4CIDRs, blockedIPv6CIDRs, err := effectiveBuildBlockedCIDRs(
		policy,
		c.cfg.NetworkBlockedIPv4CIDRs,
		c.cfg.NetworkBlockedIPv6CIDRs,
	)
	if err != nil {
		return err
	}
	script, err := nftBuildNetworkPolicyScript(
		tap,
		resolvers,
		blockedIPv4CIDRs,
		blockedIPv6CIDRs,
	)
	if err != nil {
		return err
	}
	return c.applyNetworkPolicyScript(ctx, netns, script)
}

func (c *Connector) applyNetworkPolicyScript(
	ctx context.Context,
	netns string,
	script string,
) error {
	cmd := exec.CommandContext(
		ctx,
		c.cfg.IPPath,
		"netns",
		"exec",
		netns,
		c.cfg.NFTPath,
		"-f",
		"-",
	)
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf(
				"apply firecracker network policy: %w: %s",
				err,
				detail,
			)
		}
		return fmt.Errorf("apply firecracker network policy: %w", err)
	}
	return nil
}

func (c *Connector) applyNetworkPolicy(ctx context.Context, netns string, policy compute.NetworkPolicy) error {
	blockedIPv4CIDRs, blockedIPv6CIDRs, err := effectiveBlockedCIDRs(policy, c.cfg.NetworkBlockedIPv4CIDRs, c.cfg.NetworkBlockedIPv6CIDRs)
	if err != nil {
		return err
	}
	return c.applyNetworkPolicyScript(
		ctx,
		netns,
		nftNetworkPolicyScript(
			policy,
			blockedIPv4CIDRs,
			blockedIPv6CIDRs,
		),
	)
}

func (c *Connector) cleanupNetworkPolicy(ctx context.Context, netns string) error {
	cmd := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "exec", netns, c.cfg.NFTPath, "delete", "table", "inet", networkPolicyTableName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if isMissingNetworkPolicyNamespaceOrTable(detail) {
			return nil
		}
		if detail != "" {
			return fmt.Errorf("cleanup firecracker network policy: %w: %s", err, detail)
		}
		return fmt.Errorf("cleanup firecracker network policy: %w", err)
	}
	return nil
}

func isMissingNetworkPolicyNamespaceOrTable(detail string) bool {
	detail = strings.ToLower(detail)
	return strings.Contains(detail, "no such file") ||
		strings.Contains(detail, "does not exist") ||
		strings.Contains(detail, "no such process")
}

func effectiveBlockedCIDRs(policy compute.NetworkPolicy, configuredIPv4CIDRs []string, configuredIPv6CIDRs []string) ([]string, []string, error) {
	if err := policy.Validate(); err != nil {
		return nil, nil, fmt.Errorf("firecracker network policy: %w", err)
	}
	blockedIPv4CIDRs := append([]string(nil), configuredIPv4CIDRs...)
	blockedIPv6CIDRs := append([]string(nil), configuredIPv6CIDRs...)
	for _, entry := range policy.Deny {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(entry))
		if err != nil {
			return nil, nil, fmt.Errorf("firecracker network policy deny %q: %w", entry, err)
		}
		if prefix.Addr().Is4() {
			blockedIPv4CIDRs = append(blockedIPv4CIDRs, prefix.String())
			continue
		}
		blockedIPv6CIDRs = append(blockedIPv6CIDRs, prefix.String())
	}
	return blockedIPv4CIDRs, blockedIPv6CIDRs, nil
}

func effectiveBuildBlockedCIDRs(
	policy compute.NetworkPolicy,
	configuredIPv4CIDRs []string,
	configuredIPv6CIDRs []string,
) ([]string, []string, error) {
	if err := policy.Validate(); err != nil {
		return nil, nil, fmt.Errorf("build network policy: %w", err)
	}
	if !policy.Internet ||
		len(policy.Allow) != 0 ||
		len(policy.Deny) != 0 {
		return nil, nil, errors.New(
			"build network policy must use the fixed public-egress contract",
		)
	}
	ipv4, err := canonicalPrefixSet(
		append(
			append([]string(nil), buildBlockedIPv4CIDRs...),
			configuredIPv4CIDRs...,
		),
		true,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build blocked IPv4 prefixes: %w", err)
	}
	ipv6, err := canonicalPrefixSet(
		append(
			append([]string(nil), buildBlockedIPv6CIDRs...),
			configuredIPv6CIDRs...,
		),
		false,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build blocked IPv6 prefixes: %w", err)
	}
	return ipv4, ipv6, nil
}

func canonicalPrefixSet(entries []string, ipv4 bool) ([]string, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(entry))
		if err != nil || prefix.Addr().Is4() != ipv4 {
			return nil, fmt.Errorf("invalid prefix %q", entry)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
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
	return encoded, nil
}

func buildNetworkInterface(
	machine *firecracker.Machine,
) (string, []string, error) {
	if machine == nil || len(machine.Cfg.NetworkInterfaces) != 1 {
		return "", nil, errors.New(
			"build network has no unique CNI interface",
		)
	}
	static := machine.Cfg.NetworkInterfaces[0].StaticConfiguration
	if static == nil ||
		static.HostDevName == "" ||
		static.IPConfiguration == nil ||
		len(static.IPConfiguration.Nameservers) == 0 {
		return "", nil, errors.New("build CNI network facts are incomplete")
	}
	resolvers := make([]string, 0, len(static.IPConfiguration.Nameservers))
	for _, raw := range static.IPConfiguration.Nameservers {
		address, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil || !address.IsValid() || address.IsUnspecified() {
			return "", nil, fmt.Errorf(
				"build resolver address %q is invalid",
				raw,
			)
		}
		resolvers = append(resolvers, address.String())
	}
	return static.HostDevName, resolvers, nil
}

func nftBuildNetworkPolicyScript(
	tap string,
	resolvers []string,
	blockedIPv4CIDRs []string,
	blockedIPv6CIDRs []string,
) (string, error) {
	if strings.TrimSpace(tap) == "" ||
		strings.ContainsAny(tap, "\x00\r\n") {
		return "", errors.New("build tap interface is invalid")
	}
	if len(resolvers) == 0 {
		return "", errors.New("build resolver set is empty")
	}
	ipv4Resolvers := make([]string, 0, len(resolvers))
	ipv6Resolvers := make([]string, 0, len(resolvers))
	for _, raw := range resolvers {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			return "", fmt.Errorf("build resolver address %q is invalid", raw)
		}
		if address.Is4() {
			ipv4Resolvers = append(ipv4Resolvers, address.String())
		} else {
			ipv6Resolvers = append(ipv6Resolvers, address.String())
		}
	}
	tap = strconv.Quote(tap)
	return fmt.Sprintf(strings.TrimSpace(`
add table inet %[1]s
add counter inet %[1]s %[2]s
add counter inet %[1]s %[3]s
add quota inet %[1]s %[4]s { over %[5]d mbytes }
add quota inet %[1]s %[6]s { over %[7]d mbytes }
add set inet %[1]s blocked_ipv4 { type ipv4_addr; flags interval; elements = { %[8]s } }
add set inet %[1]s blocked_ipv6 { type ipv6_addr; flags interval; elements = { %[9]s } }
add set inet %[1]s resolver_ipv4 { type ipv4_addr; elements = { %[10]s } }
add set inet %[1]s resolver_ipv6 { type ipv6_addr; elements = { %[11]s } }
add chain inet %[1]s forward { type filter hook forward priority 0; policy drop; }
add chain inet %[1]s egress
add rule inet %[1]s forward meta nfproto ipv6 counter name %[2]s drop
add rule inet %[1]s forward oifname %[12]s quota name %[4]s counter name %[3]s drop
add rule inet %[1]s forward oifname %[12]s ct state established,related accept
add rule inet %[1]s forward iifname %[12]s ip daddr @resolver_ipv4 udp dport 53 jump egress
add rule inet %[1]s forward iifname %[12]s ip daddr @resolver_ipv4 tcp dport 53 jump egress
add rule inet %[1]s forward iifname %[12]s ip daddr @blocked_ipv4 counter name %[2]s drop
add rule inet %[1]s forward iifname %[12]s tcp jump egress
add rule inet %[1]s forward counter name %[2]s drop
add rule inet %[1]s egress ct state new ct count over %[13]d counter name %[3]s drop
add rule inet %[1]s egress quota name %[6]s counter name %[3]s drop
add rule inet %[1]s egress accept
	`)+"\n",
		networkPolicyTableName,
		buildNetworkDeniedCounterName,
		buildNetworkLimitCounterName,
		buildNetworkReceivedQuotaName,
		buildNetworkReceivedLimitMiB,
		buildNetworkSentQuotaName,
		buildNetworkSentLimitMiB,
		strings.Join(blockedIPv4CIDRs, ", "),
		strings.Join(blockedIPv6CIDRs, ", "),
		strings.Join(ipv4Resolvers, ", "),
		strings.Join(ipv6Resolvers, ", "),
		tap,
		buildNetworkConnectionLimit,
	), nil
}

func nftNetworkPolicyScript(policy compute.NetworkPolicy, blockedIPv4CIDRs []string, blockedIPv6CIDRs []string) string {
	chainPolicy := "accept"
	if !policy.Internet {
		chainPolicy = "drop"
	}
	return fmt.Sprintf(strings.TrimSpace(`
add table inet %[1]s
add chain inet %[1]s forward { type filter hook forward priority 0; policy %[2]s; }
add rule inet %[1]s forward meta nfproto ipv6 drop
add rule inet %[1]s forward ct state established,related accept
%[3]s
%[4]s
add rule inet %[1]s forward ip daddr @blocked_ipv4 drop
add rule inet %[1]s forward ip6 daddr @blocked_ipv6 drop
	`)+"\n",
		networkPolicyTableName,
		chainPolicy,
		nftNetworkPolicySet("blocked_ipv4", "ipv4_addr", blockedIPv4CIDRs),
		nftNetworkPolicySet("blocked_ipv6", "ipv6_addr", blockedIPv6CIDRs),
	)
}

func (c *Connector) readBuildNetworkStatus(
	ctx context.Context,
	netns string,
) (vm.BuildNetworkStatus, error) {
	cmd := exec.CommandContext(
		ctx,
		c.cfg.IPPath,
		"netns",
		"exec",
		netns,
		c.cfg.NFTPath,
		"-j",
		"list",
		"counters",
		"table",
		"inet",
		networkPolicyTableName,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return vm.BuildNetworkStatus{}, fmt.Errorf(
				"read build network counters: %w: %s",
				err,
				detail,
			)
		}
		return vm.BuildNetworkStatus{}, fmt.Errorf(
			"read build network counters: %w",
			err,
		)
	}
	return parseBuildNetworkStatus(raw)
}

func parseBuildNetworkStatus(
	raw []byte,
) (vm.BuildNetworkStatus, error) {
	var document struct {
		Objects []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
			} `json:"counter,omitempty"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return vm.BuildNetworkStatus{}, fmt.Errorf(
			"decode build network counters: %w",
			err,
		)
	}
	var status vm.BuildNetworkStatus
	foundDenied := false
	foundLimit := false
	for _, object := range document.Objects {
		if object.Counter == nil {
			continue
		}
		switch object.Counter.Name {
		case buildNetworkDeniedCounterName:
			if foundDenied {
				return vm.BuildNetworkStatus{}, errors.New(
					"build denied counter is duplicated",
				)
			}
			foundDenied = true
			status.DeniedPackets = object.Counter.Packets
		case buildNetworkLimitCounterName:
			if foundLimit {
				return vm.BuildNetworkStatus{}, errors.New(
					"build limit counter is duplicated",
				)
			}
			foundLimit = true
			status.LimitPackets = object.Counter.Packets
		}
	}
	if !foundDenied || !foundLimit {
		return vm.BuildNetworkStatus{}, errors.New(
			"build network counters are incomplete",
		)
	}
	return status, nil
}

func nftNetworkPolicySet(name string, nftType string, cidrs []string) string {
	if len(cidrs) == 0 {
		return fmt.Sprintf("add set inet %s %s { type %s; flags interval; }", networkPolicyTableName, name, nftType)
	}
	return fmt.Sprintf(
		"add set inet %s %s { type %s; flags interval; elements = { %s } }",
		networkPolicyTableName,
		name,
		nftType,
		strings.Join(cidrs, ", "),
	)
}
