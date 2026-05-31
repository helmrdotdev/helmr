package control

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIRejectsOversizedRequestBody(t *testing.T) {
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&fakeStore{}),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
		WithSecrets(fakeSecrets{}),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(strings.Repeat("x", int(apiRequestBodyLimit)+1)))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerLogsRejectOversizedRequestBody(t *testing.T) {
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&fakeStore{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerToken := mintTestWorkerToken(t, handler, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/logs", strings.NewReader(strings.Repeat("x", int(workerLogRequestBodyLimit)+1)))
	req.Header.Set("authorization", "Bearer "+workerToken)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRecoverPanicsWritesJSONError(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("content-type"); contentType != "application/json" {
		t.Fatalf("content-type = %q", contentType)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
