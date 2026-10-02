package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestErrorPreservesMachineFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code": "idempotency_conflict", "message": "conflict",
				"details": map[string]string{"idempotency_key": "key-1"},
			},
		})
	}))
	defer server.Close()

	transport, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	req, err := transport.Request(context.Background(), http.MethodGet, "/resource", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	err = transport.DoJSON(req, nil)
	var httpError *Error
	if !errors.As(err, &httpError) || httpError.Code != "idempotency_conflict" ||
		httpError.Message != "conflict" ||
		string(httpError.Details) != `{"idempotency_key":"key-1"}` {
		t.Fatalf("error = %#v", err)
	}
}

// Preparation callers retain the status; ordinary callers retain their read error.
func TestErrorReadFailureStatusIsOptIn(t *testing.T) {
	for _, status := range []int{409, 503} {
		for _, retain := range []bool{false, true} {
			t.Run(http.StatusText(status)+"/retain="+strconv.FormatBool(retain), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, b, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					b.WriteString("HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\nContent-Length: 1000\r\n\r\n{")
					b.Flush()
					conn.Close()
				}))
				defer server.Close()
				transport, err := New(server.URL, server.Client())
				if err != nil {
					t.Fatal(err)
				}
				req, err := transport.Request(t.Context(), http.MethodPost, "/resource", nil, "")
				if err != nil {
					t.Fatal(err)
				}
				if retain {
					_, err = transport.DoWithStatus(req)
				} else {
					_, err = transport.Do(req)
				}
				if IsStatus(err, status) != retain || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("retain=%v status or cause changed: %v", retain, err)
				}
				if retain && !strings.Contains(err.Error(), http.StatusText(status)) {
					t.Fatalf("status missing from diagnostics: %v", err)
				}
			})
		}
	}
}
