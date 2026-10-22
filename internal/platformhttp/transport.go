// Package platformhttp constructs operator-controlled HTTP paths for platform
// services. It does not enforce destination policy; the deployment network does.
package platformhttp

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Direct ignores ambient proxy variables. Private services and AWS endpoints
// must not accidentally inherit the external integration proxy.
func Direct() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// External uses exactly the configured path, including after redirects and
// connection failures. An empty URL explicitly selects direct access.
func External(raw string) (*http.Transport, error) {
	t := Direct()
	if raw == "" {
		return t, nil
	}
	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(raw) != raw || u.Hostname() == "" ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Path != "" ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		// Never include a proxy URL: it can contain authentication credentials.
		return nil, errors.New("OUTBOUND_PROXY_URL must be an absolute HTTP(S) proxy origin without path, query or fragment")
	}
	if port := u.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return nil, errors.New("OUTBOUND_PROXY_URL has an invalid port")
		}
	}
	t.Proxy = http.ProxyURL(u)
	return t, nil
}
