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
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestEmailProviderNoneDisablesDebugLogMailer(t *testing.T) {
	handler := control.New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		control.WithDB(&emptyStore{}),
		control.WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		control.WithMagicLinkDebugURLs(true),
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
