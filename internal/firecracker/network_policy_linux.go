//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
)

const networkPolicyTableName = "helmr_network_policy"

func (c *Connector) withNetworkPolicy(netns string) fc.Opt {
	return func(machine *fc.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(fc.SetupNetworkHandlerName, fc.Handler{
			Name: "fcinit.ApplyHelmrNetworkPolicy",
			Fn: func(ctx context.Context, _ *fc.Machine) error {
				return c.applyNetworkPolicy(ctx, netns)
			},
		})
	}
}

func (c *Connector) applyNetworkPolicy(ctx context.Context, netns string) error {
	cmd := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "exec", netns, c.cfg.NFTPath, "-f", "-")
	cmd.Stdin = strings.NewReader(nftNetworkPolicyScript(c.cfg.NetworkBlockedIPv4CIDRs, c.cfg.NetworkBlockedIPv6CIDRs))
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

func nftNetworkPolicyScript(blockedIPv4CIDRs []string, blockedIPv6CIDRs []string) string {
	return fmt.Sprintf(strings.TrimSpace(`
add table inet %[1]s
add chain inet %[1]s forward { type filter hook forward priority 0; policy accept; }
%[2]s
%[3]s
add rule inet %[1]s forward ct state established,related accept
add rule inet %[1]s forward ip daddr @blocked_ipv4 drop
add rule inet %[1]s forward ip6 daddr @blocked_ipv6 drop
add rule inet %[1]s forward udp dport 53 accept
add rule inet %[1]s forward tcp dport 53 accept
	`)+"\n",
		networkPolicyTableName,
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
