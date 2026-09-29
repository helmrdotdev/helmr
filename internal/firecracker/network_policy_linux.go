//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Only the connector-owned qualification probe has no control-plane reservation.
// Every real runtime must complete preparation, including Computers with no bindings.
func (c *Connector) prepareSecretTransport(ctx context.Context, mode launchMode, runtimeID string, blocked []netip.Prefix) (*secretproxy.Proxy, error) {
	if mode == startupProbeLaunch {
		return nil, nil
	}
	if c.cfg.PrepareSecretTransport == nil {
		return nil, errors.New("computer Secret transport preparation is not configured")
	}
	return c.cfg.PrepareSecretTransport(ctx, runtimeID, blocked)
}

func (c *Connector) installRoutedPolicy(ctx context.Context, mode launchMode, binding *installedNetworkBinding) error {
	m := binding.manifest
	guestIP := strings.Split(GuestNetworkCIDRV0, "/")[0]
	hostIPv4, err := hostIPv4Prefixes(netip.MustParsePrefix(GuestNetworkCIDRV0).Masked())
	if err != nil {
		return err
	}
	linkPool, _, err := configuredNetworkPools(c.cfg)
	if err != nil {
		return err
	}
	blocked := append(hostIPv4, linkPool)
	blocked = append(blocked, c.cfg.NetworkBlockedIPv4CIDRs...)
	// Preparation precedes ResumeVM, but grants no credential-use authority.
	{
		proxy, prepareErr := c.prepareSecretTransport(ctx, mode, m.OwnerID, blocked)
		if prepareErr != nil {
			return fmt.Errorf("prepare Computer Secret transport: %w", prepareErr)
		}
		binding.secretProxy = proxy
		if proxy != nil {
			var listener net.Listener
			if err := withNetworkNamespace(binding.namespace, func() error {
				var e error
				listener, e = listenSecretEgress()
				return e
			}); err != nil {
				if listener != nil {
					listener.Close()
				}
				proxy.Close()
				return err
			}
			// Serve on ordinary host threads: outbound DNS and sockets stay in host namespace.
			go func() {
				if e := proxy.Serve(listener); e != nil {
					select {
					case binding.failure <- errors.New("computer Secret transport stopped"):
					default:
					}
				}
			}()
		}
	}
	var packet *datapath.Binding
	if err := withNetworkNamespace(binding.namespace, func() error {
		var prepareErr error
		guestIPv4 := netip.MustParsePrefix(GuestNetworkCIDRV0).Addr()
		gatewayIPv4 := netip.MustParseAddr(GuestGatewayIPv4V0)
		guestMAC, _ := net.ParseMAC(GuestMACV0)
		packet, prepareErr = c.datapath.Prepare(datapath.Authority{
			WorkerEpoch: binding.manifest.WorkerEpoch,
			OwnerID:     binding.manifest.OwnerID,
			Generation:  binding.manifest.Generation,
		}, datapath.InterfaceFacts{TapName: m.TapName, GuestIPv4: guestIPv4, GatewayIPv4: gatewayIPv4, GuestMAC: guestMAC})
		return prepareErr
	}); err != nil {
		return fmt.Errorf("prepare TAP ingress binding: %w", err)
	}
	binding.packet = packet
	if binding.secretProxy != nil {
		if err := withNetworkNamespace(binding.namespace, func() error { return installSecretRoute(m.TapName, packet.Mark()) }); err != nil {
			return err
		}
	}
	script, err := renderNetworkPolicy(networkPolicyInput{
		Tap: m.TapName, Peer: m.NamespaceVethName, Mark: packet.Mark(),
		BlockedIPv4CIDRs: blocked,
		ProtectedPorts:   protectedPorts(binding.secretProxy),
		ResolverIPv4:     c.cfg.NetworkResolverIPv4,
		GuestIPv4:        guestIP, TranslationIPv4: netip.MustParsePrefix(m.TranslationIPv4CIDR).Addr().String(),
	})
	if err != nil {
		return err
	}
	if err := c.applyNftScript(ctx, m.NamespaceName, script); err != nil {
		return err
	}
	rootScript := renderRootNetworkPolicy(m)
	if err := c.applyNftScript(ctx, "", rootScript); err != nil {
		return err
	}
	binding.namespacePolicyFingerprint, err = c.nftTableFingerprint(ctx, m.NamespaceName, networkPolicyTableName)
	if err != nil {
		return err
	}
	binding.rootPolicyFingerprint, err = c.nftTableFingerprint(ctx, "", m.RootTableName)
	if err != nil {
		return err
	}
	return nil
}

func installRoutedRoutes(binding *installedNetworkBinding) error {
	m := binding.manifest
	nsHandle, err := netlink.NewHandleAt(binding.namespace)
	if err != nil {
		return fmt.Errorf("open namespace for route preparation: %w", err)
	}
	defer nsHandle.Close()
	nsVeth, err := nsHandle.LinkByName(m.NamespaceVethName)
	if err != nil {
		return fmt.Errorf("find namespace veth for routes: %w", err)
	}
	rootVeth, err := netlink.LinkByName(m.RootVethName)
	if err != nil {
		return fmt.Errorf("find root veth for routes: %w", err)
	}
	rootIP := net.ParseIP(netip.MustParsePrefix(m.RootIPv4CIDR).Addr().String()).To4()
	if err := nsHandle.RouteAdd(&netlink.Route{LinkIndex: nsVeth.Attrs().Index, Gw: rootIP, Flags: int(unix.RTNH_F_ONLINK)}); err != nil {
		return fmt.Errorf("install namespace default route: %w", err)
	}
	nsIP := net.ParseIP(netip.MustParsePrefix(m.NamespaceIPv4CIDR).Addr().String()).To4()
	translationIP := net.ParseIP(netip.MustParsePrefix(m.TranslationIPv4CIDR).Addr().String()).To4()
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: rootVeth.Attrs().Index, Gw: nsIP, Dst: &net.IPNet{IP: translationIP, Mask: net.CIDRMask(32, 32)}, Flags: int(unix.RTNH_F_ONLINK)}); err != nil {
		return fmt.Errorf("install translation route: %w", err)
	}
	return nil
}

func renderRootNetworkPolicy(m networkOwnerManifest) string {
	table := m.RootTableName
	rootVeth := strconv.Quote(m.RootVethName)
	var script strings.Builder
	fmt.Fprintf(&script, "add table inet %s\n", table)
	fmt.Fprintf(&script, "add chain inet %s input { type filter hook input priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s output { type filter hook output priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s forward { type filter hook forward priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s postrouting { type nat hook postrouting priority srcnat; policy accept; }\n", table)
	fmt.Fprintf(&script, "add rule inet %s input iifname %s drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s output oifname %s drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname \"hr*\" drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s accept\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward oifname %s ct state established,related accept\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward oifname %s drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s postrouting ip saddr %s oifname != %s masquerade\n", table, netip.MustParsePrefix(m.TranslationIPv4CIDR).Addr(), rootVeth)
	return script.String()
}

func (c *Connector) applyNftScript(ctx context.Context, namespace string, script string) error {
	args := []string{"-f", "-"}
	command := c.cfg.NFTPath
	if namespace != "" {
		command = c.cfg.IPPath
		args = append([]string{"netns", "exec", namespace, c.cfg.NFTPath}, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apply network policy: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *Connector) deleteNftTable(ctx context.Context, namespace string, table string) error {
	args := []string{"delete", "table", "inet", table}
	command := c.cfg.NFTPath
	if namespace != "" {
		command = c.cfg.IPPath
		args = append([]string{"netns", "exec", namespace, c.cfg.NFTPath}, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.ToLower(stderr.String())
		if strings.Contains(detail, "no such file") || strings.Contains(detail, "does not exist") || strings.Contains(detail, "no such process") {
			return nil
		}
		return fmt.Errorf("delete network policy table %s: %w: %s", table, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *Connector) nftTableFingerprint(ctx context.Context, namespace string, table string) (string, error) {
	args := []string{"-j", "list", "table", "inet", table}
	command := c.cfg.NFTPath
	if namespace != "" {
		command = c.cfg.IPPath
		args = append([]string{"netns", "exec", namespace, c.cfg.NFTPath}, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		detail := strings.ToLower(stderr.String())
		if strings.Contains(detail, "no such file") || strings.Contains(detail, "does not exist") || strings.Contains(detail, "no such process") {
			return "", fmt.Errorf("read network policy table %s: %w", table, os.ErrNotExist)
		}
		return "", fmt.Errorf("read network policy table %s: %w: %s", table, err, strings.TrimSpace(stderr.String()))
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", fmt.Errorf("decode network policy table %s: %w", table, err)
	}
	normalized := normalizeNftDocument(document, "")
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func normalizeNftDocument(value any, parent string) any {
	switch typed := value.(type) {
	case []any:
		for index := range typed {
			typed[index] = normalizeNftDocument(typed[index], parent)
		}
		return typed
	case map[string]any:
		delete(typed, "handle")
		if parent == "counter" {
			delete(typed, "packets")
			delete(typed, "bytes")
		}
		if parent == "quota" {
			delete(typed, "used")
		}
		for key, child := range typed {
			typed[key] = normalizeNftDocument(child, key)
		}
		return typed
	default:
		return value
	}
}
