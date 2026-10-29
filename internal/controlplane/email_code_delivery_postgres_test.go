package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/identity"
)

func TestEmailCodeDeliveryPostgresShutdownFailsActiveAndQueuedRows(t *testing.T) {
	sender := &blockingEmailCodeSender{started: make(chan email.Message, emailCodeDeliveryWorkers)}
	delivery := NewEmailCodeDelivery(discardEmailCodeLog(), 10*time.Second)
	fixture := newEmailCodeDeliveryPostgresFixture(t, sender, delivery)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()

	for index := range emailCodeDeliveryWorkers {
		startLoginEmailCode(t, fixture, fmt.Sprintf("active-%d@example.test", index))
	}
	for range emailCodeDeliveryWorkers {
		select {
		case <-sender.started:
		case <-time.After(time.Second):
			t.Fatal("active delivery did not start")
		}
	}
	for index := range emailCodeDeliveryQueue {
		startLoginEmailCode(t, fixture, fmt.Sprintf("queued-%d@example.test", index))
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
		  FROM email_challenges
	`).Scan(&failed, &sent); err != nil {
		t.Fatal(err)
	}
	wantFailed := emailCodeDeliveryWorkers + emailCodeDeliveryQueue
	if failed != wantFailed || sent != 0 {
		t.Fatalf("failed/sent rows = %d/%d, want %d/0", failed, sent, wantFailed)
	}
}

func TestEmailCodeLoginQueueSaturationRemainsNonEnumerating(t *testing.T) {
	sender := &blockingEmailCodeSender{started: make(chan email.Message, 1)}
	delivery := newEmailCodeDelivery(discardEmailCodeLog(), 1, 1, time.Second)
	fixture := newEmailCodeDeliveryPostgresFixture(t, sender, delivery)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	startLoginEmailCode(t, fixture, "active@example.test")
	select {
	case <-sender.started:
	case <-time.After(time.Second):
		t.Fatal("active delivery did not start")
	}
	startLoginEmailCode(t, fixture, "queued@example.test")

	response := startLoginEmailCode(t, fixture, "saturated@example.test")
	if response.Code != "" || response.Email != "" || response.ID.String() == "" {
		t.Fatalf("response = %+v, want challenge without secret or address", response)
	}
	var failed bool
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT delivery_failed_at IS NOT NULL
		  FROM email_challenges
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

func TestEmailCodeDeliveryUsesBoundedWorker(t *testing.T) {
	sender := &recordingEmailCodeSender{sent: make(chan email.Message, 1)}
	delivery := newEmailCodeDelivery(discardEmailCodeLog(), 1, 1, time.Second)
	fixture := newEmailCodeDeliveryPostgresFixture(t, sender, delivery)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	challenge := startLoginEmailCode(t, fixture, "delivery@example.test")
	select {
	case message := <-sender.sent:
		if message.To != "delivery@example.test" || message.PlainText == "" {
			t.Fatalf("wrong delivery: %+v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery did not use the worker")
	}
	// Wait for the worker's delivery acknowledgement before checking eligibility.
	deadline := time.Now().Add(time.Second)
	for {
		var sent bool
		if err := fixture.pool.QueryRow(t.Context(), `SELECT sent_at IS NOT NULL FROM email_challenges WHERE id=$1`, challenge.ID).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if sent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successful delivery was not acknowledged")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
}

type blockingEmailCodeSender struct {
	email.Unconfigured
	started chan email.Message
}

func (s *blockingEmailCodeSender) SendEmail(ctx context.Context, message email.Message) error {
	select {
	case s.started <- message:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

type recordingEmailCodeSender struct {
	email.Unconfigured
	sent chan email.Message
}

func (s *recordingEmailCodeSender) SendEmail(_ context.Context, message email.Message) error {
	s.sent <- message
	return nil
}

func newEmailCodeDeliveryPostgresFixture(t *testing.T, sender email.Sender, delivery *EmailCodeDelivery) httpPostgresFixture {
	t.Helper()
	return newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		cfg.Mailer = sender
		cfg.EmailCodeDelivery = delivery
	})
}

// startLoginEmailCode requests a browser-bound challenge without exposing its code.
func startLoginEmailCode(t *testing.T, fixture httpPostgresFixture, address string) identity.EmailChallenge {
	t.Helper()
	body, err := json.Marshal(api.EmailStartRequest{Email: address, Next: "/"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := fixture.request(t, http.MethodPost, "/api/auth/email/start", "", string(body))
	var response identity.EmailChallenge
	if recorder.Code != http.StatusAccepted || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		t.Fatalf("email challenge start = %d %s", recorder.Code, recorder.Body.String())
	}
	return response
}
