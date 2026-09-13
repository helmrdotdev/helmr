package workerclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// PrepareSecretTransport is host-only. The immutable runtime locator is closed
// over by every callback; no guest identity or decrypted value is cached.
func (c *Client) PrepareSecretTransport(ctx context.Context, runtimeID string, blocked []netip.Prefix) (*secretproxy.Proxy, error) {
	request := workerapi.SecretProxyRequest{RuntimeInstanceID: runtimeID}
	prepared, err := c.PrepareSecretProxy(ctx, request)
	if err != nil {
		return nil, err
	}
	defer clear(prepared.PrivateKey)
	if len(prepared.Origins) == 0 {
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
	certificate, err := parse(prepared)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	dialer := &secretproxy.Dialer{Blocked: append([]netip.Prefix(nil), blocked...)}
	return secretproxy.New(secretproxy.Config{Origins: prepared.Origins, DialContext: dialer.DialContext,
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
			result, e := c.ResolveSecretProxy(ctx, workerapi.SecretProxyRequest{RuntimeInstanceID: runtimeID, Origin: origin, Placeholders: markers})
			return result.Values, e
		}})
}
