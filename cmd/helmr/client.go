package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cli/session"
	"github.com/helmrdotdev/helmr/internal/client"
)

const (
	helmrURLEnv    = "HELMR_URL"
	helmrAPIKeyEnv = "HELMR_API_KEY"
)

var newSessionStore = session.New

func controlClient() (*client.Client, error) {
	rawURL := strings.TrimSpace(os.Getenv(helmrURLEnv))
	bearer := strings.TrimSpace(os.Getenv(helmrAPIKeyEnv))
	var state *session.Store
	if rawURL == "" || bearer == "" {
		var err error
		state, err = newSessionStore()
		if err != nil && rawURL == "" {
			return nil, err
		}
	}
	if rawURL == "" && state != nil {
		cfg, err := state.Load()
		if err == nil {
			rawURL = cfg.DefaultHost
		} else if !errors.Is(err, session.ErrNotFound) {
			return nil, err
		}
	}
	parsed, err := parseControlURL(rawURL)
	if err != nil {
		if rawURL == "" {
			return nil, fmt.Errorf("helmr API access requires %s=http(s)://... or helmr login", helmrURLEnv)
		}
		return nil, err
	}
	baseURL := parsed.String()
	if bearer == "" && state != nil {
		stored, err := state.Token(baseURL)
		if err == nil {
			bearer = stored
		} else if !errors.Is(err, session.ErrNotFound) {
			return nil, err
		}
	}
	if bearer == "" {
		return nil, fmt.Errorf("helmr API access requires %s or helmr login", helmrAPIKeyEnv)
	}
	return client.New(baseURL, client.WithBearerToken(bearer))
}

func parseControlURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("control URL is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", helmrURLEnv, rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported %s scheme %q; expected http or https", helmrURLEnv, parsed.Scheme)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("base URL must not include query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("refusing to send %s over plaintext non-loopback URL %s", helmrAPIKeyEnv, parsed.Redacted())
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
