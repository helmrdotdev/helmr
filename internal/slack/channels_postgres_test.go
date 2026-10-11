package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"
)

func verifiedChannelBody(channel string) map[string]any {
	return map[string]any{"id": channel, "name": "work", "context_team_id": "T1", "is_channel": true, "is_member": true, "is_shared": false}
}
func channelClient(t *testing.T, store *CredentialStore, body map[string]any, before func()) *WebClient {
	t.Helper()
	return NewWebClient(store, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Host != "slack.com" || r.URL.Path != "/api/conversations.info" || r.Header.Get("Authorization") != "Bearer old-access" {
			t.Errorf("unexpected channel verification request: %s", r.URL)
		}
		if before != nil {
			before()
		}
		raw, _ := json.Marshal(map[string]any{"ok": true, "channel": body})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	}))
}

func TestSlackChannelAccessRejectsUnsupportedDestinations(t *testing.T) {
	for _, kind := range []string{"valid", "not-member", "shared", "im", "archived", "wrong-team", "wrong-id"} {
		t.Run(kind, func(t *testing.T) {
			f, store, _ := credentialFixture(t)
			body := verifiedChannelBody("C1")
			body["context_team_id"] = "team"
			switch kind {
			case "not-member":
				body["is_member"] = false
			case "shared":
				body["is_ext_shared"] = true
			case "im":
				body["is_im"] = true
			case "archived":
				body["is_archived"] = true
			case "wrong-team":
				body["context_team_id"] = "another-team"
			case "wrong-id":
				body["id"] = "COTHER"
			}
			_, err := channelClient(t, store, body, nil).verifyChannel(t.Context(), f.installation, 1, "team", "C1")
			if kind == "valid" && err != nil {
				t.Fatal(err)
			}
			if kind != "valid" && !errors.Is(err, ErrChannelVerification) {
				t.Fatalf("destination accepted: %v", err)
			}
		})
	}
}

type admissionCredentials struct{}

func (admissionCredentials) BotToken(context.Context, uuid.UUID, int64) (string, error) {
	return "test-token", nil
}
func (f statusFixture) admissionClient(t *testing.T) *WebClient {
	t.Helper()
	return NewWebClient(admissionCredentials{}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && r.URL.Path == "/api/conversations.history" {
			raw, _ := json.Marshal(map[string]any{"ok": true, "messages": []any{map[string]any{"ts": r.URL.Query().Get("latest"), "user": "human", "text": "original request"}}})
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
		}
		if r.Method != "GET" || r.URL.Path != "/api/conversations.info" {
			t.Fatalf("unexpected admission request: %s", r.URL)
		}
		body := verifiedChannelBody(r.URL.Query().Get("channel"))
		body["context_team_id"] = "team"
		raw, _ := json.Marshal(map[string]any{"ok": true, "channel": body})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	}))
}
