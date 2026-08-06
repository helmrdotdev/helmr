package controlplane

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestRequestCorrelationGeneratesServerOwnedUUIDv7(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.requestCorrelation(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(requestIDHeader, "caller-controlled")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	requestID := response.Header().Get(requestIDHeader)
	parsed, err := uuid.Parse(requestID)
	if err != nil {
		t.Fatalf("%s = %q: %v", requestIDHeader, requestID, err)
	}
	if parsed.Version() != 7 {
		t.Fatalf("request ID version = %d, want 7", parsed.Version())
	}
	if requestID == "caller-controlled" {
		t.Fatal("request ID trusted caller input")
	}
}

func TestRequestCorrelationCoversRecoveredPanics(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.recoverPanics(server.requestCorrelation(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { panic("boom") },
	)))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if _, err := uuid.Parse(response.Header().Get(requestIDHeader)); err != nil {
		t.Fatalf("%s is not a UUID: %v", requestIDHeader, err)
	}
}
