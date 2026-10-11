package slack

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"
)

func TestSlackHistoryReadPinsAuthenticatedQueryAndExposesPagination(t *testing.T) {
	for _, thread := range []string{"", "123.456"} {
		t.Run(thread, func(t *testing.T) {
			calls := 0
			client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				method := "conversations.history"
				if thread != "" {
					method = "conversations.replies"
				}
				query := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "slack.com" || r.URL.Path != "/api/"+method || query.Get("channel") != "C1" || query.Get("ts") != thread || query.Get("cursor") != "cursor+value" || query.Get("limit") != "15" || query.Get("include_all_metadata") != "true" || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Fatal("history destination/authority changed")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"has_more":true,"messages":[{"channel":"untrusted","app_id":"app","user":"bot","ts":"123.789","text":"hello","metadata":{"event_type":"helmr_post"}}],"response_metadata":{"next_cursor":"next"}}`))}, nil
			}))
			page := client.readMessages(t.Context(), uuid.NewV7(), 1, "C1", thread, "cursor+value")
			if page.Code != "" || len(page.Messages) != 1 || page.Messages[0].Channel != "C1" || page.NextCursor != "next" || !page.More || calls != 1 {
				t.Fatalf("page %+v calls %d", page, calls)
			}
		})
	}
}

func TestSlackHistoryMissingOrFailedReadProvidesNoMutationEvidence(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
		code   string
		retry  bool
	}{
		{`{"ok":true,"messages":[]}`, 200, "", false},
		{`{"ok":false,"error":"missing_scope"}`, 200, "history_unavailable", false},
		{`{"ok":false,"error":"rate_limited"}`, 200, "rate_limited", true},
		{`{}`, 429, "rate_limited", true},
		{`{"ok":true,"ok":false}`, 200, "invalid_history_response", false},
		{`{"ok":true,"response_metadata":{"warnings":["incomplete"]}}`, 200, "history_incomplete", false},
	} {
		client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{"90"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		}))
		page := client.readMessages(t.Context(), uuid.NewV7(), 1, "C1", "", "")
		if page.Code != tc.code || len(page.Messages) != 0 || (tc.retry && page.RetryAfter != 90*time.Second) {
			t.Fatalf("failed/empty page produced proof: %+v", page)
		}
	}
}
