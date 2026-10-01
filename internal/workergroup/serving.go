package workergroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
)

const servingProbeTimeout = 5 * time.Second

var errServingEvidenceExpired = errors.New("control plane serving evidence expired")

func controlPlaneServingProbe(baseURL string) (func(context.Context) error, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Each sample exercises the current DNS/TLS path instead of a retained connection.
	transport.DisableKeepAlives = true
	client := &http.Client{Transport: transport, Timeout: servingProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	endpoint, err := httpclient.New(baseURL, client)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		request, err := endpoint.Request(ctx, http.MethodGet, "/readyz", nil, "")
		if err != nil {
			return err
		}
		request.Header.Set("Cache-Control", "no-cache")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("readiness HTTP status %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 1025))
		if err != nil {
			return err
		}
		var readiness struct {
			Status string `json:"status"`
		}
		if len(body) > 1024 || json.Unmarshal(body, &readiness) != nil || readiness.Status != "ready" {
			return errors.New("invalid control plane readiness response")
		}
		return nil
	}, nil
}

func (f *StaleHostFencer) servingEvidenceMaxAge() time.Duration { return f.every + servingProbeTimeout }

// Both clocks must agree on freshness: monotonic time catches wall-clock steps,
// and wall time catches host suspension on platforms whose monotonic clock stops.
func servingSampleFresh(monotonicAge, wallAge, maxAge time.Duration) bool {
	return min(monotonicAge, wallAge) >= 0 && max(monotonicAge, wallAge) <= maxAge
}

func (f *StaleHostFencer) servingWindowReady(now time.Time) bool {
	return !f.servingSince.IsZero() &&
		min(now.Sub(f.servingSince), now.Round(0).Sub(f.servingSince.Round(0))) >= time.Duration(ObservationFreshnessSeconds)*time.Second &&
		servingSampleFresh(now.Sub(f.lastServingAt), now.Round(0).Sub(f.lastServingAt.Round(0)), f.servingEvidenceMaxAge())
}

func (f *StaleHostFencer) suspendCycle(cycle StaleHostFenceCycle, reason string, details ...any) StaleHostFenceCycle {
	now := f.clock.Now()
	if reason != f.suspensionReason || now.Sub(f.lastSuspensionLog) >= time.Minute {
		f.log.Warn("stale worker fencing suspended", append([]any{"reason", reason, "healthy_since", f.servingSince}, details...)...)
		f.lastSuspensionLog = now
	}
	f.suspensionReason = reason
	cycle.Suspended = true
	cycle.SuspensionReason = reason
	return cycle
}
