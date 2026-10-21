package slack

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/definition"
)

func directClient(t *testing.T, peer string, mutation func(*http.Request) string) *WebClient {
	t.Helper()
	return NewWebClient(admissionCredentials{}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		if r.URL.Path == "/api/conversations.info" {
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Error("wrong token")
			}
			raw, _ := json.Marshal(map[string]any{"ok": true, "channel": map[string]any{"id": r.URL.Query().Get("channel"), "is_im": true, "user": peer}})
			body = string(raw)
		} else if mutation != nil {
			body = mutation(r)
		} else {
			t.Errorf("unexpected Slack call %s", r.URL.Path)
			body = `{"ok":false}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
}

func TestDirectMessageVerificationRequiresExactPeer(t *testing.T) {
	for _, kind := range []string{"valid", "missing-peer", "wrong-peer", "mpim", "shared", "wrong-team", "wrong-id", "channel"} {
		t.Run(kind, func(t *testing.T) {
			body := map[string]any{"id": "D1", "is_im": true, "user": "human"}
			switch kind {
			case "missing-peer":
				delete(body, "user")
			case "wrong-peer":
				body["user"] = "other"
			case "mpim":
				body["is_mpim"] = true
			case "shared":
				body["is_ext_shared"] = true
			case "wrong-team":
				body["context_team_id"] = "other"
			case "wrong-id":
				body["id"] = "D2"
			case "channel":
				body["is_im"] = false
				body["is_channel"] = true
			}
			c := NewWebClient(admissionCredentials{}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw, _ := json.Marshal(map[string]any{"ok": true, "channel": body})
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			}))
			_, err := c.verifyConversation(t.Context(), uuid.NewV7(), 1, "team", "D1", "human")
			if (err == nil) != (kind == "valid") {
				t.Fatal(kind, err)
			}
			if _, err = c.verifyConversation(t.Context(), uuid.NewV7(), 1, "team", "D1", ""); err == nil {
				t.Fatal("DM accepted without peer authority")
			}
		})
	}
	if definition.ValidSlackChannelID("D1") {
		t.Fatal("public destination accepted DM")
	}
}

func TestDirectMessageInputPreservesMentionText(t *testing.T) {
	input, err := messageInput(messageGesture{Channel: "D1", Text: "<@bot> explain &amp; help"}, "bot")
	if err != nil {
		t.Fatal(err)
	}
	var parts []map[string]string
	if err = json.Unmarshal(input, &parts); err != nil || len(parts) != 1 || parts[0]["text"] != "<@bot> explain & help" {
		t.Fatal(string(input), err)
	}
}
