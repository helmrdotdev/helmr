//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/vm"
)

const (
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

func (c *Connector) withNetworkPolicy(netns string) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.SetupNetworkHandlerName, firecracker.Handler{
			Name: "fcinit.ApplyHelmrNetworkPolicy",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
				if c.kernelArgsValue() == buildKernelArgs {
					tap, resolvers, err := buildNetworkInterface(machine)
					if err != nil {
						return err
					}
					return c.applyBuildNetworkPolicy(
						ctx,
						netns,
						tap,
						resolvers,
					)
				}
				return c.applyNetworkPolicy(ctx, netns)
			},
		})
	}
}

func (c *Connector) applyBuildNetworkPolicy(
	ctx context.Context,
	netns string,
	tap string,
	resolvers []string,
) error {
	blockedIPv4CIDRs, err := effectiveBuildBlockedCIDRs(
		c.cfg.NetworkBlockedIPv4CIDRs,
	)
	if err != nil {
		return err
	}
	script, err := nftBuildNetworkPolicyScript(
		tap,
		resolvers,
		blockedIPv4CIDRs,
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

func (c *Connector) applyNetworkPolicy(ctx context.Context, netns string) error {
	return c.applyNetworkPolicyScript(
		ctx,
		netns,
		renderRunNetworkPolicy(
			c.cfg.NetworkBlockedIPv4CIDRs,
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

func effectiveBuildBlockedCIDRs(
	configuredIPv4CIDRs []string,
) ([]string, error) {
	ipv4, err := canonicalPrefixSet(
		append(
			append([]string(nil), buildBlockedIPv4CIDRs...),
			configuredIPv4CIDRs...,
		),
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("build blocked IPv4 prefixes: %w", err)
	}
	return ipv4, nil
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
) (string, error) {
	if strings.TrimSpace(tap) == "" ||
		strings.ContainsAny(tap, "\x00\r\n") {
		return "", errors.New("build tap interface is invalid")
	}
	if len(resolvers) == 0 {
		return "", errors.New("build resolver set is empty")
	}
	ipv4Resolvers := make([]string, 0, len(resolvers))
	for _, raw := range resolvers {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			return "", fmt.Errorf("build resolver address %q is invalid", raw)
		}
		if address.Is4() {
			ipv4Resolvers = append(ipv4Resolvers, address.String())
		}
	}
	if len(ipv4Resolvers) == 0 {
		return "", errors.New("build IPv4 resolver set is empty")
	}
	tap = strconv.Quote(tap)
	return fmt.Sprintf(strings.TrimSpace(`
add table inet %[1]s
add counter inet %[1]s %[2]s
add counter inet %[1]s %[3]s
add quota inet %[1]s %[4]s { over %[5]d mbytes }
add quota inet %[1]s %[6]s { over %[7]d mbytes }
add set inet %[1]s blocked_ipv4 { type ipv4_addr; flags interval; elements = { %[8]s } }
add set inet %[1]s resolver_ipv4 { type ipv4_addr; elements = { %[9]s } }
add chain inet %[1]s forward { type filter hook forward priority 0; policy drop; }
add chain inet %[1]s egress
add rule inet %[1]s forward meta nfproto ipv6 counter name %[2]s drop
add rule inet %[1]s forward oifname %[10]s quota name %[4]s counter name %[3]s drop
add rule inet %[1]s forward oifname %[10]s ct state established,related accept
add rule inet %[1]s forward iifname %[10]s ip daddr @resolver_ipv4 udp dport 53 jump egress
add rule inet %[1]s forward iifname %[10]s ip daddr @resolver_ipv4 tcp dport 53 jump egress
add rule inet %[1]s forward iifname %[10]s ip daddr @blocked_ipv4 counter name %[2]s drop
add rule inet %[1]s forward iifname %[10]s tcp jump egress
add rule inet %[1]s forward counter name %[2]s drop
add rule inet %[1]s egress ct state new ct count over %[11]d counter name %[3]s drop
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
		strings.Join(ipv4Resolvers, ", "),
		tap,
		buildNetworkConnectionLimit,
	), nil
}

func (c *Connector) readNetworkCounters(
	ctx context.Context,
	netns string,
	label string,
) ([]byte, error) {
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
			return nil, fmt.Errorf(
				"read %s network counters: %w: %s",
				label,
				err,
				detail,
			)
		}
		return nil, fmt.Errorf(
			"read %s network counters: %w",
			label,
			err,
		)
	}
	return raw, nil
}

func (c *Connector) readRunNetworkStatus(
	ctx context.Context,
	netns string,
) (vm.RunNetworkStatus, error) {
	raw, err := c.readNetworkCounters(ctx, netns, "Run")
	if err != nil {
		return vm.RunNetworkStatus{}, err
	}
	return parseRunNetworkStatus(raw)
}

func (c *Connector) readBuildNetworkStatus(
	ctx context.Context,
	netns string,
) (vm.BuildNetworkStatus, error) {
	raw, err := c.readNetworkCounters(ctx, netns, "build")
	if err != nil {
		return vm.BuildNetworkStatus{}, err
	}
	return parseBuildNetworkStatus(raw)
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
