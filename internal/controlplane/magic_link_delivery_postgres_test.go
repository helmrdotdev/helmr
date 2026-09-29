package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/email"
)

func TestMagicLinkDeliveryPostgresShutdownFailsActiveAndQueuedRows(t *testing.T) {
	sender := &blockingMagicLinkSender{started: make(chan email.Message, magicLinkDeliveryWorkers)}
	delivery := NewMagicLinkDelivery(discardMagicLinkLog(), 10*time.Second)
	fixture := newMagicLinkDeliveryPostgresFixture(t, sender, delivery, false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()

	for index := range magicLinkDeliveryWorkers {
		startLoginMagicLink(t, fixture, fmt.Sprintf("active-%d@example.test", index))
	}
	for range magicLinkDeliveryWorkers {
		select {
		case <-sender.started:
		case <-time.After(time.Second):
			t.Fatal("active delivery did not start")
		}
	}
	for index := range magicLinkDeliveryQueue {
		startLoginMagicLink(t, fixture, fmt.Sprintf("queued-%d@example.test", index))
	}
	shutdownStarted := time.Now()
	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", runErr)
	}
	if elapsed := time.Since(shutdownStarted); elapsed > 10*time.Second {
		t.Fatalf("shutdown duration = %s, want <= 10s", elapsed)
	}
	var failed, sent int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE delivery_failed_at IS NOT NULL),
		       count(*) FILTER (WHERE sent_at IS NOT NULL)
		  FROM magic_links
	`).Scan(&failed, &sent); err != nil {
		t.Fatal(err)
	}
	wantFailed := magicLinkDeliveryWorkers + magicLinkDeliveryQueue
	if failed != wantFailed || sent != 0 {
		t.Fatalf("failed/sent rows = %d/%d, want %d/0", failed, sent, wantFailed)
	}
}

func TestMagicLinkLoginQueueSaturationRemainsNonEnumerating(t *testing.T) {
	sender := &blockingMagicLinkSender{started: make(chan email.Message, 1)}
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 1, 1, time.Second)
	fixture := newMagicLinkDeliveryPostgresFixture(t, sender, delivery, false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	startLoginMagicLink(t, fixture, "active@example.test")
	select {
	case <-sender.started:
	case <-time.After(time.Second):
		t.Fatal("active delivery did not start")
	}
	startLoginMagicLink(t, fixture, "queued@example.test")

	response := startLoginMagicLink(t, fixture, "saturated@example.test")
	if !response.Sent || response.Email != "" || response.DebugURL != "" {
		t.Fatalf("response = %+v, want non-enumerating sent response", response)
	}
	var failed bool
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT delivery_failed_at IS NOT NULL
		  FROM magic_links
		 WHERE email = 'saturated@example.test'
	`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if !failed {
		t.Fatal("saturated delivery row was left pending")
	}
	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", runErr)
	}
}

func TestMagicLinkDebugDeliveryUsesBoundedWorker(t *testing.T) {
	sender := &recordingMagicLinkSender{sent: make(chan email.Message, 1)}
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 1, 1, time.Second)
	fixture := newMagicLinkDeliveryPostgresFixture(t, sender, delivery, true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	debugURL := startLoginMagicLink(t, fixture, "debug@example.test").DebugURL
	if debugURL == "" {
		t.Fatal("debug URL was not returned")
	}
	select {
	case <-sender.sent:
	case <-time.After(time.Second):
		t.Fatal("debug delivery did not use the worker")
	}
	parsedURL, err := url.Parse(debugURL)
	if err != nil {
		t.Fatal(err)
	}
	tokenHash, err := auth.HashToken(fixture.keys.MagicLink, parsedURL.Query().Get("token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetActiveMagicLinkByTokenHash(t.Context(), tokenHash); err != nil {
		t.Fatalf("debug URL was not immediately usable after return: %v", err)
	}
	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", runErr)
	}
}

type blockingMagicLinkSender struct {
	started chan email.Message
}

func (s *blockingMagicLinkSender) SendEmail(ctx context.Context, message email.Message) error {
	select {
	case s.started <- message:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

type recordingMagicLinkSender struct {
	sent chan email.Message
}

func (s *recordingMagicLinkSender) SendEmail(_ context.Context, message email.Message) error {
	s.sent <- message
	return nil
}

func newMagicLinkDeliveryPostgresFixture(t *testing.T, sender email.Sender, delivery *MagicLinkDelivery, debug bool) httpPostgresFixture {
	t.Helper()
	return newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		cfg.Mailer = sender
		cfg.MagicLinkDelivery = delivery
		cfg.MagicLinkDebugURLs = debug
	})
}

// startLoginMagicLink requests a login magic link, which always answers 200.
func startLoginMagicLink(t *testing.T, fixture httpPostgresFixture, address string) api.MagicLinkStartResponse {
	t.Helper()
	body, err := json.Marshal(api.MagicLinkStartRequest{Email: address, Next: "/"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := fixture.request(t, http.MethodPost, "/api/auth/magic-link/start", "", string(body))
	var response api.MagicLinkStartResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		t.Fatalf("magic link start = %d %s", recorder.Code, recorder.Body.String())
	}
	return response
}
