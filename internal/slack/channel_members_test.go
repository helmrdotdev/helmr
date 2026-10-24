package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"
)

func TestChannelMembershipRequiresCurrentMemberAcrossPages(t *testing.T) {
	for _, tc := range []struct {
		name, second         string
		allowed, unavailable bool
	}{
		{"later page", `{"ok":true,"members":["actor"]}`, true, false},
		{"absent", `{"ok":true,"members":[]}`, false, false},
		{"provider error", `{"ok":false,"error":"missing_scope"}`, false, true},
		{"repeated cursor", `{"ok":true,"members":[],"response_metadata":{"next_cursor":"page2"}}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/api/conversations.members" || r.URL.Query().Get("channel") != "C1" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				body := `{"ok":true,"members":["other"],"response_metadata":{"next_cursor":"page2"}}`
				if calls == 2 {
					if r.URL.Query().Get("cursor") != "page2" {
						t.Fatal("missing cursor")
					}
					body = tc.second
				}
				if calls > 2 {
					t.Fatal("repeated page")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			err := c.verifyChannelMember(t.Context(), "fixture", "C1", "actor")
			var failure *channelVerificationFailure
			if (err == nil) != tc.allowed || errors.As(err, &failure) != tc.unavailable || calls != 2 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if !tc.allowed && !tc.unavailable && !errors.Is(err, ErrChannelVerification) {
				t.Fatal(err)
			}
		})
	}
}
