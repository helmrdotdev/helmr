package config

import (
	"errors"
	"net/http"
	"os"

	"github.com/helmrdotdev/helmr/internal/httpclient"
)

// AccessHTTPClient configures the optional outer authentication gateway for
// process-owned control plane requests. Credentials are scoped independently of
// the selected API URL so changing that URL cannot redirect secrets elsewhere.
func AccessHTTPClient(base *http.Client) (*http.Client, error) {
	origin := os.Getenv("HELMR_ACCESS_ORIGIN")
	id := os.Getenv("HELMR_ACCESS_CLIENT_ID")
	secret := os.Getenv("HELMR_ACCESS_CLIENT_SECRET")
	if origin == "" && id == "" && secret == "" {
		return base, nil
	}
	if origin == "" || id == "" || secret == "" {
		return nil, errors.New("HELMR_ACCESS_ORIGIN, HELMR_ACCESS_CLIENT_ID and HELMR_ACCESS_CLIENT_SECRET must be set together")
	}
	return httpclient.WithAccessServiceToken(base, origin, id, secret)
}
