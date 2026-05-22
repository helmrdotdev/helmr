package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/server"
)

func TestEmailProviderNoneDisablesDebugLogMailer(t *testing.T) {
	handler := server.New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		server.WithDB(&emptyStore{}),
		server.WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		server.WithMagicLinkDebugURLs(true),
		emailSenderOption(config.Control{EmailProvider: config.EmailProviderNone}),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/start", bytes.NewBufferString(`{"email":"user@example.test"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "magic link mailer is not configured") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

type emptyStore struct {
	db.Querier
}
