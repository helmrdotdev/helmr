package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeStartTaskRequestIsClosedAndPayloadPresenceAware(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"payload":null,"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}}`),
	)
	decoded, payloadPresent, err := decodeStartTaskRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !payloadPresent || string(decoded.Payload) != "null" {
		t.Fatalf("payload present=%v value=%s", payloadPresent, decoded.Payload)
	}

	for _, body := range []string{
		`null`,
		`{}`,
		`{"options":null}`,
		`{"computer":null}`,
		`{"computer":{"key":"computer:1"}}`,
		`{"computer":{"key":null}}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"unknown":true}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":null}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"idempotency_key":""}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"queue":""}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"ttl":""}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"metadata":null}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"tags":[null]}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"retry":{"enabled":null}}`,
		`{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},"retry":{"backoff":{"factor":null}}}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, _, err := decodeStartTaskRequest(request); err == nil {
			t.Fatalf("decodeStartTaskRequest(%s) succeeded", body)
		}
	}
}
