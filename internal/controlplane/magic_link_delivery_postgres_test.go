package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestMagicLinkDeliveryPostgresShutdownFailsActiveAndQueuedRows(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	sender := &blockingMagicLinkSender{started: make(chan email.Message, magicLinkDeliveryWorkers)}
	delivery := NewMagicLinkDelivery(discardMagicLinkLog(), 10*time.Second)
	server := newMagicLinkDeliveryPostgresServer(t, database, sender, delivery, false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()

	request := httptest.NewRequest("POST", "/api/auth/magic-link/start", nil)
	for index := range magicLinkDeliveryWorkers {
		address := fmt.Sprintf("active-%d@example.test", index)
		if _, err := server.sendMagicLink(request, db.MagicLinkPurposeLogin, address, pgtype.UUID{}, pgtype.UUID{}, "/"); err != nil {
			t.Fatal(err)
		}
	}
	for range magicLinkDeliveryWorkers {
		select {
		case <-sender.started:
		case <-time.After(time.Second):
			t.Fatal("active delivery did not start")
		}
	}
	for index := range magicLinkDeliveryQueue {
		address := fmt.Sprintf("queued-%d@example.test", index)
		if _, err := server.sendMagicLink(request, db.MagicLinkPurposeLogin, address, pgtype.UUID{}, pgtype.UUID{}, "/"); err != nil {
			t.Fatal(err)
		}
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
	if err := database.Pool.QueryRow(t.Context(), `
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
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	sender := &blockingMagicLinkSender{started: make(chan email.Message, 1)}
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 1, 1, time.Second)
	server := newMagicLinkDeliveryPostgresServer(t, database, sender, delivery, false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	request := httptest.NewRequest("POST", "/api/auth/magic-link/start", nil)
	if _, err := server.sendMagicLink(request, db.MagicLinkPurposeLogin, "active@example.test", pgtype.UUID{}, pgtype.UUID{}, "/"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sender.started:
	case <-time.After(time.Second):
		t.Fatal("active delivery did not start")
	}
	if _, err := server.sendMagicLink(request, db.MagicLinkPurposeLogin, "queued@example.test", pgtype.UUID{}, pgtype.UUID{}, "/"); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.magicLinkLoginStart(recorder, request, api.MagicLinkStartRequest{Email: "saturated@example.test"})
	if recorder.Code != 200 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.MagicLinkStartResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Sent || response.Email != "" || response.DebugURL != "" {
		t.Fatalf("response = %+v, want non-enumerating sent response", response)
	}
	var failed bool
	if err := database.Pool.QueryRow(t.Context(), `
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
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	sender := &recordingMagicLinkSender{sent: make(chan email.Message, 1)}
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 1, 1, time.Second)
	server := newMagicLinkDeliveryPostgresServer(t, database, sender, delivery, true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	request := httptest.NewRequest("POST", "/api/auth/magic-link/start", nil)
	debugURL, err := server.sendMagicLink(request, db.MagicLinkPurposeLogin, "debug@example.test", pgtype.UUID{}, pgtype.UUID{}, "/")
	if err != nil {
		t.Fatal(err)
	}
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
	tokenHash, err := auth.HashToken(server.authKeys.MagicLink, parsedURL.Query().Get("token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.GetActiveMagicLinkByTokenHash(t.Context(), tokenHash); err != nil {
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

func newMagicLinkDeliveryPostgresServer(
	t *testing.T,
	database dbtest.Database,
	sender email.Sender,
	delivery *MagicLinkDelivery,
	debug bool,
) *Server {
	t.Helper()
	publicURL, err := url.Parse("https://helmr.example.test")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(make([]byte, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:                 db.New(database.Pool),
		tx:                 database.Pool,
		authKeys:           keys,
		publicURL:          publicURL,
		mailer:             sender,
		magicLinkDelivery:  delivery,
		magicLinkDebugURLs: debug,
	}
}
