package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
