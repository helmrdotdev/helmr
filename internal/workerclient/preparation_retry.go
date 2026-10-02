package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
)

// preparationRequest replays only immutable Instance preparation requests. It
// encodes once, refreshes rejected credentials once, and makes at most three
// transient attempts within the caller's preparation deadline. Each response
// is fully read and closed before retry; sensitive failures retain no details.
func (c *Client) preparationRequest(ctx context.Context, path string, input any, sensitive bool, limit int64) ([]byte, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode preparation request: %w", err)
	}
	refreshed := false
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		credential, err := c.hostCredential(ctx)
		authFailed := err != nil
		var body []byte
		readFailed := false
		if err == nil {
			var req *http.Request
			req, err = c.transport.Request(ctx, http.MethodPost, path, bytes.NewReader(payload), credential)
			if err != nil {
				return nil, err
			}
			req.Header.Set("content-type", "application/json")
			var response *http.Response
			if sensitive {
				response, err = c.transport.DoSensitive(req)
			} else {
				response, err = c.transport.DoWithStatus(req)
			}
			if err == nil {
				var reader io.Reader = response.Body
				if limit > 0 {
					reader = io.LimitReader(reader, limit+1)
				}
				body, err = io.ReadAll(reader)
				response.Body.Close()
				readFailed = err != nil
				if err == nil && limit > 0 && int64(len(body)) > limit {
					clear(body)
					return nil, errors.New("preparation response exceeds size limit")
				}
			}
		}
		if err == nil {
			return body, nil
		}
		clear(body)
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		if !authFailed && !refreshed && httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateHostCredential(credential)
			refreshed = true
			continue
		}
		var transportErr *url.Error
		transient := readFailed || errors.Is(err, httpclient.ErrSensitiveTransport) || errors.As(err, &transportErr)
		var rejected *httpclient.Error
		if errors.As(err, &rejected) {
			transient = rejected.StatusCode == http.StatusServiceUnavailable
		}
		if !transient || failures == 2 {
			if sensitive && authFailed {
				return nil, errors.New("computer key authentication failed")
			}
			if sensitive && readFailed {
				return nil, errors.New("invalid computer key response")
			}
			return nil, err
		}
		failures++
		timer := time.NewTimer(time.Duration(failures) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) postPreparationJSON(ctx context.Context, path string, input any, out any) error {
	body, err := c.preparationRequest(ctx, path, input, false, 0)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
