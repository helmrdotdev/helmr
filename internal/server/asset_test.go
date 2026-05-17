//go:build embed_console

package server

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/console"
)

func TestConsoleFallbackServesIndex(t *testing.T) {
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/auth/device?user_code=ABCD-EFGH", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("cache-control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	if got := rec.Header().Get("referrer-policy"); got != "no-referrer" {
		t.Fatalf("referrer-policy = %q", got)
	}
	if got := rec.Header().Get("content-type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", got)
	}
}

func TestConsoleAssetsUseImmutableCache(t *testing.T) {
	dist := console.FS()
	matches, err := fs.Glob(dist, "assets/*.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no built javascript asset found")
	}

	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/"+matches[0], nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("cache-control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("cache-control = %q", got)
	}
	if got := rec.Header().Get("content-type"); got != "text/javascript; charset=utf-8" {
		t.Fatalf("content-type = %q", got)
	}
}

func TestUnknownAPIPathReturnsJSONNotIndex(t *testing.T) {
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/api/missing", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
}

func TestMissingConsoleAssetReturnsNotFound(t *testing.T) {
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
