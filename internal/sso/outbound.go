package sso

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

const (
	outboundTimeout = 10 * time.Second
	// maxOutboundBody bounds discovery, key and token responses.
	maxOutboundBody = 1 << 20
	maxRedirects    = 3
)

var errPrivateDestination = errors.New("identity provider address is not public")

// NewHTTPClient returns the client used for requests to an organization's
// identity provider: discovery, signing keys and code exchange. Requests use
// HTTPS, time out and are size-bounded. Managed Cloud refuses private, loopback
// and link-local destinations so an administrator-supplied issuer cannot reach
// internal services; self-host allows them because private IdPs are expected.
func NewHTTPClient(allowPrivate bool) *http.Client { return newHTTPClient(allowPrivate, nil) }

// newHTTPClient takes extra trust roots for tests' local TLS servers.
func newHTTPClient(allowPrivate bool, roots *x509.CertPool) *http.Client {
	dialer := &net.Dialer{Timeout: outboundTimeout}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !publicIP(ip) {
				return errPrivateDestination
			}
			return nil
		}
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   outboundTimeout,
		ResponseHeaderTimeout: outboundTimeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       time.Minute,
	}
	return &http.Client{
		Timeout:   outboundTimeout,
		Transport: limitedTransport{transport},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many identity provider redirects")
			}
			if r.URL.Scheme != "https" {
				return fmt.Errorf("identity provider redirect to %s is not HTTPS", r.URL.Scheme)
			}
			return nil
		},
	}
}

// nonPublic are address ranges an IdP on the public internet never uses:
// private, shared (CGNAT), loopback, link-local, documentation, benchmarking,
// reserved and IPv6 transition ranges that embed IPv4.
var nonPublic = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32",
		"2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		ps = append(ps, netip.MustParsePrefix(p))
	}
	return ps
}()

func publicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range nonPublic {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

type limitedTransport struct{ next http.RoundTripper }

func (t limitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("identity provider URL %s is not HTTPS", r.URL.Redacted())
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = limitedBody{Reader: io.LimitReader(resp.Body, maxOutboundBody), Closer: resp.Body}
	return resp, nil
}

type limitedBody struct {
	io.Reader
	io.Closer
}
