package workerclient

import (
	"bufio"
	"context"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/httpclient"
)

// WatchUpdates returns on disconnect; the host reconnects and catches up from
// durable allocation state. Hints contain no allocation or execution authority.
func (c *Client) WatchUpdates(ctx context.Context, wake func()) error {
	credential, err := c.hostCredential(ctx)
	if err != nil {
		return err
	}
	req, err := c.transport.Request(ctx, http.MethodGet, "/worker/v1/updates", nil, credential)
	if err != nil {
		return err
	}
	response, err := c.transport.Do(req)
	if err != nil {
		if httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateHostCredential(credential)
		}
		return err
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 1024), 4096)
	for scanner.Scan() {
		if scanner.Text() == "event: change" {
			wake()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}
