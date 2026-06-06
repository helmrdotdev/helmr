//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/compute"
)

const networkPolicyTableName = "helmr_network_policy"

func (c *Connector) withNetworkPolicy(netns string, policy compute.NetworkPolicy) fc.Opt {
	return func(machine *fc.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(fc.SetupNetworkHandlerName, fc.Handler{
			Name: "fcinit.ApplyHelmrNetworkPolicy",
			Fn: func(ctx context.Context, _ *fc.Machine) error {
				return c.applyNetworkPolicy(ctx, netns, policy)
			},
		})
	}
}

func (c *Connector) applyNetworkPolicy(ctx context.Context, netns string, policy compute.NetworkPolicy) error {
	cmd := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "exec", netns, c.cfg.NFTPath, "-f", "-")
	blockedIPv4CIDRs, blockedIPv6CIDRs, err := effectiveBlockedCIDRs(policy, c.cfg.NetworkBlockedIPv4CIDRs, c.cfg.NetworkBlockedIPv6CIDRs)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(nftNetworkPolicyScript(policy, blockedIPv4CIDRs, blockedIPv6CIDRs))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("apply firecracker network policy: %w: %s", err, detail)
		}
		return fmt.Errorf("apply firecracker network policy: %w", err)
	}
	return nil
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

func nftNetworkPolicyScript(policy compute.NetworkPolicy, blockedIPv4CIDRs []string, blockedIPv6CIDRs []string) string {
	chainPolicy := "accept"
	if !policy.Internet {
		chainPolicy = "drop"
	}
	return fmt.Sprintf(strings.TrimSpace(`
add table inet %[1]s
add chain inet %[1]s forward { type filter hook forward priority 0; policy %[2]s; }
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
