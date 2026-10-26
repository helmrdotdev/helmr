package secretproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"
)

func (p *Proxy) serveRestricted(client net.Conn, destination netip.AddrPort) {
	if destination.Port() != 443 {
		p.config.Decisions.record(p.ctx, "tls", "", destination.Addr(), destination.Port(), false, "port_denied")
		return
	}
	_ = client.SetReadDeadline(time.Now().Add(classificationTimeout))
	host, records, err := inspectClientHello(client)
	if err != nil {
		p.config.Decisions.record(p.ctx, "tls", "", destination.Addr(), destination.Port(), false, "client_hello_rejected")
		return
	}
	if !p.config.NetworkPolicy.AllowsHostname(host) {
		p.config.Decisions.record(p.ctx, "tls", host, destination.Addr(), destination.Port(), false, "hostname_denied")
		return
	}
	_ = client.SetReadDeadline(time.Time{})
	// The guest's original numeric address is never an upstream authority.
	dialRequest := func(ctx context.Context) (net.Conn, error) { return p.dialHostname(ctx, host, 443) }
	replay := &readerConn{Conn: client, reader: io.MultiReader(bytes.NewReader(records), client)}
	target := "https://" + host
	if slices.Contains(p.config.Origins, target) {
		p.serveProtected(client, replay, host, target, destination, dialRequest)
		return
	}
	upstream, err := dialRequest(p.ctx)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.relay(replay, upstream, nil, nil, closedSignal(), closedSignal())
}
func (p *Proxy) dialHostname(ctx context.Context, host string, port uint16) (net.Conn, error) {
	if port != 443 || !p.config.NetworkPolicy.AllowsHostname(host) {
		p.config.Decisions.record(ctx, "tls", host, netip.Addr{}, port, false, "hostname_denied")
		return nil, errors.New("network hostname denied")
	}
	ctx, cancel := context.WithTimeout(ctx, classificationTimeout)
	defer cancel()
	addresses, err := p.config.LookupIP(ctx, "ip4", host+".")
	if err != nil {
		p.config.Decisions.record(ctx, "tls", host, netip.Addr{}, port, false, "resolution_failed")
		return nil, err
	}
	for _, ip := range addresses {
		if !p.config.NetworkPolicy.AllowsAddress(host, ip, p.config.Installation) {
			p.config.Decisions.record(ctx, "tls", host, ip, port, false, "address_denied")
			continue
		}
		p.config.Decisions.record(ctx, "tls", host, ip, port, true, "destination_allowed")
		conn, err := p.dial(ctx, net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	p.config.Decisions.record(ctx, "tls", host, netip.Addr{}, port, false, "no_available_destination")
	return nil, errors.New("network destination unavailable")
}
