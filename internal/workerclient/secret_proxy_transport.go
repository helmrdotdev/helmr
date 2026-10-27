package workerclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net"
)

// PrepareSecretTransport is host-only. The immutable instance locator is closed
// over by every callback; no guest identity or decrypted value is cached.
func (c *Client) PrepareSecretTransport(ctx context.Context, instanceID string, policy networkpolicy.Policy, installation networkpolicy.Installation, resolver string) (*secretproxy.Proxy, error) {
	request := workerapi.SecretProxyRequest{AllocationID: instanceID}
	prepared, err := c.PrepareSecretProxy(ctx, request)
	if err != nil {
		return nil, err
	}
	defer clear(prepared.PrivateKey)
	if err := networkpolicy.ValidateInstallation(installation); err != nil {
		return nil, err
	}
	for _, origin := range prepared.Origins {
		if err := policy.ValidateProtectedOrigin(origin); err != nil {
			return nil, err
		}
	}
	if len(prepared.Origins) == 0 && policy.Kind != networkpolicy.Allowlist {
		return nil, nil
	}
	parse := func(value workerapi.SecretProxyPreparation) (tls.Certificate, error) {
		cert, e := tls.X509KeyPair(value.Certificate, value.PrivateKey)
		if e != nil {
			return cert, e
		}
		cert.Leaf, e = x509.ParseCertificate(cert.Certificate[0])
		return cert, e
	}
	var certificate tls.Certificate
	if len(prepared.Origins) > 0 {
		certificate, err = parse(prepared)
	}
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	dialer := &secretproxy.Dialer{Installation: &installation}
	trusted := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(resolver, "53"))
	}}
	return secretproxy.New(secretproxy.Config{NetworkPolicy: policy, Installation: installation, LookupIP: trusted.LookupNetIP, Origins: prepared.Origins, DialContext: dialer.DialContext, AllowedDestination: dialer.Allowed,
		Certificate: func(ctx context.Context, host string) (tls.Certificate, error) {
			mu.Lock()
			defer mu.Unlock()
			if time.Until(certificate.Leaf.NotAfter) < time.Minute {
				renewed, e := c.PrepareSecretProxy(ctx, request)
				if e != nil {
					return tls.Certificate{}, e
				}
				defer clear(renewed.PrivateKey)
				next, e := parse(renewed)
				if e != nil {
					return tls.Certificate{}, e
				}
				certificate = next
			}
			if certificate.Leaf.VerifyHostname(host) != nil {
				return tls.Certificate{}, errors.New("secret transport host unavailable")
			}
			return certificate, nil
		}, Resolve: func(ctx context.Context, origin string, markers []string) (map[string][]byte, error) {
			result, e := c.ResolveSecretProxy(ctx, workerapi.SecretProxyRequest{AllocationID: instanceID, Origin: origin, Placeholders: markers})
			return result.Values, e
		}})
}
