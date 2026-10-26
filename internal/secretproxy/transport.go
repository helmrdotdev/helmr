package secretproxy

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"net"
	"net/netip"
	"time"
)

// Dialer preserves the connector's effective IPv4 deny set for host-originated IO.
// Numeric destinations are policy checked and never re-resolved.
type Dialer struct {
	Installation *networkpolicy.Installation
	dialer       net.Dialer
}

func (d *Dialer) Allowed(ip netip.Addr) bool {
	return d.Installation != nil && d.Installation.AllowsAddress(ip)
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	destination, err := netip.ParseAddrPort(address)
	if err != nil || network != "tcp4" || destination.Port() == 0 || !d.Allowed(destination.Addr()) {
		return nil, errors.New("egress destination is not an allowed numeric IPv4 endpoint")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// The guest already resolved its destination. No host DNS lookup can redirect
	// this connection. TLS authenticates the service name at the pinned address.
	return d.dialer.DialContext(ctx, "tcp4", destination.String())
}
