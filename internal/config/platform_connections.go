package config

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/platformhttp"
)

func validatePlatformConnections(slackTransport, proxyURL string) error {
	switch slackTransport {
	case "disabled", "http", "socket":
	default:
		return errors.New("SLACK_TRANSPORT must be disabled, http or socket")
	}
	_, err := platformhttp.External(proxyURL)
	return err
}
