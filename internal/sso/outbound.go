package sso

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast())
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
