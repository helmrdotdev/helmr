package secretproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"
)

// Dialer preserves the connector's effective IPv4 deny set for host-originated IO.
// Resolve and Dial are deliberately not public configuration escape hatches.
type Dialer struct {
	Blocked  []netip.Prefix
	resolver *net.Resolver
	dialer   net.Dialer
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
}

func (d *Dialer) Allowed(ip netip.Addr) bool {
	if !ip.Is4() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, prefixes := range [][]netip.Prefix{nonPublic, d.Blocked} {
		for _, prefix := range prefixes {
			if prefix.Contains(ip) {
				return false
			}
		}
	}
	return true
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid proxy destination")
	}
	resolver := d.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := resolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("proxy destination has no usable IPv4 address")
	}
	for _, ip := range ips {
		if !d.Allowed(ip) {
			return nil, errors.New("proxy destination is blocked")
		}
	}
	// Pin the checked address. Never resolve the hostname a second time in Dial.
	return d.dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ips[0].String(), port))
}
