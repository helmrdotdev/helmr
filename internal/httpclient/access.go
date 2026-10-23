package httpclient

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// WithAccessServiceToken sends a Cloudflare Access service token only to origin.
// It preserves application authorization and never sends the token to redirect
// destinations or signed storage URLs on another origin.
func WithAccessServiceToken(client *http.Client, origin, id, secret string) (*http.Client, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Access service token origin must be an HTTPS origin")
	}
	if id == "" || secret == "" || strings.ContainsAny(id+secret, " \t\r\n") {
		return nil, errors.New("Access service token requires a valid client ID and secret")
	}
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	base := copied.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copied.Transport = &accessTransport{base: base, host: strings.ToLower(u.Host), id: id, secret: secret}
	return &copied, nil
}

type accessTransport struct {
	base             http.RoundTripper
	host, id, secret string
}

func (t *accessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copied := req.Clone(req.Context())
	if copied.Header == nil {
		copied.Header = make(http.Header)
	}
	copied.Header.Del("CF-Access-Client-Id")
	copied.Header.Del("CF-Access-Client-Secret")
	if copied.URL.Scheme == "https" && strings.EqualFold(copied.URL.Host, t.host) && (copied.Host == "" || strings.EqualFold(copied.Host, t.host)) {
		copied.Header.Set("CF-Access-Client-Id", t.id)
		copied.Header.Set("CF-Access-Client-Secret", t.secret)
	}
	return t.base.RoundTrip(copied)
}
