package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/clistate"
	"github.com/spf13/cobra"
)

const (
	helmrAPIURLEnv = "HELMR_API_URL"
	helmrAPIKeyEnv = "HELMR_API_KEY"
)

type cliState interface {
	Load() (clistate.Config, error)
	Token(string) (string, error)
	SaveLogin(string, string) error
	DeleteToken(string) error
}

var newCLIStateStore = func() (cliState, error) {
	return clistate.New()
}

func controlPlaneClient(cmd *cobra.Command) (*client.Client, error) {
	rawURL := cliControlPlaneURL(cmd)
	bearer := strings.TrimSpace(os.Getenv(helmrAPIKeyEnv))
	sessionScopedRoutes := false
	var state cliState
	if rawURL == "" || bearer == "" {
		var err error
		state, err = newCLIStateStore()
		if err != nil && rawURL == "" {
			return nil, err
		}
	}
	if rawURL == "" && state != nil {
		cfg, err := state.Load()
		if err == nil {
			rawURL = cfg.DefaultHost
		} else if !errors.Is(err, clistate.ErrNotFound) {
			return nil, err
		}
	}
	parsed, err := parseControlPlaneURL(rawURL)
	if err != nil {
		if rawURL == "" {
			return nil, fmt.Errorf("access to the Helmr API requires %s=http(s)://... or helmr login", helmrAPIURLEnv)
		}
		return nil, err
	}
	baseURL := parsed.String()
	if bearer == "" && state != nil {
		stored, err := state.Token(baseURL)
		if err == nil {
			bearer = stored
			sessionScopedRoutes = true
		} else if !errors.Is(err, clistate.ErrNotFound) {
			return nil, err
		}
	}
	if bearer == "" {
		return nil, fmt.Errorf("access to the Helmr API requires %s or helmr login", helmrAPIKeyEnv)
	}
	opts := []client.Option{client.WithBearerToken(bearer)}
	if sessionScopedRoutes {
		opts = append(opts, client.WithSessionScopedRoutes())
	}
	return client.New(baseURL, opts...)
}

func sessionControlPlaneClient(cmd *cobra.Command) (*client.Client, error) {
	rawURL := cliControlPlaneURL(cmd)
	state, err := newCLIStateStore()
	if err != nil {
		return nil, err
	}
	if rawURL == "" {
		cfg, err := state.Load()
		if err == nil {
			rawURL = cfg.DefaultHost
		} else if errors.Is(err, clistate.ErrNotFound) {
			return nil, fmt.Errorf("project and environment management requires helmr login")
		} else {
			return nil, err
		}
	}
	parsed, err := parseControlPlaneURL(rawURL)
	if err != nil {
		return nil, err
	}
	baseURL := parsed.String()
	bearer, err := state.Token(baseURL)
	if err != nil {
		if errors.Is(err, clistate.ErrNotFound) {
			return nil, fmt.Errorf("project and environment management requires helmr login")
		}
		return nil, err
	}
	return client.New(baseURL, client.WithBearerToken(bearer), client.WithSessionScopedRoutes())
}

func cliControlPlaneURL(cmd *cobra.Command) string {
	if rawURL := explicitAPIURL(cmd); rawURL != "" {
		return rawURL
	}
	return strings.TrimSpace(os.Getenv(helmrAPIURLEnv))
}

func explicitAPIURL(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	root := cmd.Root()
	if root == nil {
		return ""
	}
	flag := root.PersistentFlags().Lookup("api-url")
	if flag == nil || !flag.Changed {
		return ""
	}
	rawURL, err := root.PersistentFlags().GetString("api-url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(rawURL)
}

func parseControlPlaneURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("control plane URL is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", helmrAPIURLEnv, rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported %s scheme %q; expected http or https", helmrAPIURLEnv, parsed.Scheme)
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
