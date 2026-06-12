package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestEmailProviderNoneDisablesDebugLogMailer(t *testing.T) {
	store := &emptyStore{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	publicURL, err := url.Parse("https://helmr.example.test")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := control.NewServer(control.ServerConfig{
		Log:                log,
		DB:                 store,
		Auth:               auth.NewDBAuthenticator(store),
		AuthSecret:         []byte("abcdefghijabcdefghijabcdefghij12"),
		PublicURL:          publicURL,
		MagicLinkDebugURLs: true,
		Mailer:             configuredEmailSender(log, config.Control{EmailProvider: config.EmailProviderNone}),
	})
	if err != nil {
		t.Fatal(err)
	}
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
