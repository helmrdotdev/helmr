package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/url"
	"strings"

	"github.com/helmrdotdev/helmr/internal/agent"
)

var ErrOAuthAuthorization = errors.New("the Slack authorization could not be completed; start authorization again")

func IsInstallationDenied(err error) bool { return errors.Is(err, errInstallationAuthority) }

// ControlKey derives the signed interaction capability key from the same
// platform root already entrusted to this adapter, with a separate domain.
func ControlKey(root []byte) ([]byte, error) {
	if len(root) != 32 {
		return nil, errors.New("the Slack control root key must be 32 bytes")
	}
	mac := hmac.New(sha256.New, root)
	_, _ = mac.Write([]byte("helmr.slack-controls.v1"))
	return mac.Sum(nil), nil
}

func (c *OAuthClient) AuthorizationURL(state, redirect string) string {
	query := url.Values{"client_id": {c.clientID}, "scope": {strings.Join(agent.RequiredSlackScopes(), ",")}, "state": {state}, "redirect_uri": {redirect}}
	return "https://slack.com/oauth/v2/authorize?" + query.Encode()
}
