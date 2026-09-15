//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const secretRouteTable = 100
const secretRoutePriority = 100

func protectedPorts(proxy *secretproxy.Proxy) []uint16 {
	if proxy == nil {
		return nil
	}
	return proxy.Ports()
}

// Invoke in the runtime namespace; the socket retains it when Serve runs on
// ordinary worker threads. Upstream sockets deliberately use the host namespace.
func listenSecretEgress() (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fd uintptr) { socketErr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_TRANSPARENT, 1) }); err != nil {
			return err
		}
		return socketErr
	}}
	return config.Listen(context.Background(), "tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(secretproxy.Port)))
}

func secretRule(tap string, mark uint32) *netlink.Rule {
	rule := netlink.NewRule()
	rule.Family = unix.AF_INET
	rule.Priority = secretRoutePriority
	rule.Table = secretRouteTable
	rule.IifName = tap
	rule.Mark = secretRouteMark(mark)
	mask := uint32(0xffffffff)
	rule.Mask = &mask
	return rule
}

func installSecretRoute(tap string, mark uint32) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	route := netlink.Route{Table: secretRouteTable, LinkIndex: lo.Attrs().Index, Type: unix.RTN_LOCAL, Scope: netlink.SCOPE_HOST, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}
	if err := netlink.RouteAdd(&route); err != nil {
		return fmt.Errorf("install protected egress local route: %w", err)
	}
	if err := netlink.RuleAdd(secretRule(tap, mark)); err != nil {
		return fmt.Errorf("install protected egress routing rule: %w", err)
	}
	return nil
}

func verifySecretRoute(tap string, mark uint32) error {
	rules, err := netlink.RuleList(unix.AF_INET)
	if err != nil {
		return err
	}
	found := 0
	for _, rule := range rules {
		if rule.Priority == secretRoutePriority {
			found++
			if rule.Table != secretRouteTable || rule.IifName != tap || rule.Mark != secretRouteMark(mark) || rule.Mask == nil || *rule.Mask != 0xffffffff || rule.Src != nil || rule.Dst != nil || rule.Invert || rule.Goto >= 0 || rule.OifName != "" {
				return errors.New("protected egress routing rule changed")
			}
		} else if rule.Priority != 0 && rule.Priority != 32766 && rule.Priority != 32767 {
			return errors.New("unexpected protected egress routing rule")
		}
	}
	if found != 1 {
		return errors.New("protected egress routing rule missing or duplicated")
	}
	routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: secretRouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	if len(routes) != 1 {
		return errors.New("protected egress local route missing or duplicated")
	}
	route := routes[0]
	if !isDefaultIPv4Route(route.Dst) || route.Type != unix.RTN_LOCAL || route.Scope != netlink.SCOPE_HOST || route.LinkIndex != lo.Attrs().Index || len(route.Gw) != 0 {
		return errors.New("protected egress local route changed")
	}
	return nil
}

// Qualify kernel support before reserving a real runtime. All probe state lives
// in an unnamed disposable namespace, including its socket, route and nft table.
func (c *Connector) checkSecretEgressKernel(ctx context.Context) error {
	current, err := netns.Get()
	if err != nil {
		return err
	}
	defer current.Close()
	return withNetworkNamespace(current, func() error {
		scratch, err := netns.New()
		if err != nil {
			return err
		}
		defer scratch.Close()
		listener, err := listenSecretEgress()
		if err != nil {
			return fmt.Errorf("protected egress requires IP_TRANSPARENT: %w", err)
		}
		defer listener.Close()
		if err := installSecretRoute("lo", 71); err != nil {
			return err
		}
		command := exec.CommandContext(ctx, c.cfg.NFTPath, "-f", "-")
		command.Stdin = strings.NewReader("add table inet helmr_egress_probe\nadd chain inet helmr_egress_probe capture { type filter hook prerouting priority mangle; policy accept; }\nadd rule inet helmr_egress_probe capture meta l4proto tcp tproxy ip to :3128 accept\n")
		if out, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("protected egress requires nftables TPROXY: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	})
}
