package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"
)

type credentialsFunc func(context.Context, uuid.UUID, int64) (string, error)

func (f credentialsFunc) BotToken(ctx context.Context, id uuid.UUID, ref int64) (string, error) {
	return f(ctx, id, ref)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebClientMakesOneExactMutationAndClassifiesAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		body           string
		transportError bool
		want           Disposition
	}{
		{"ack", 200, `{"ok":true,"channel":"C1","ts":"123.456"}`, false, Acknowledged},
		{"missing-id", 200, `{"ok":true}`, false, Uncertain},
		{"wrong-channel", 200, `{"ok":true,"channel":"C2","ts":"123.456"}`, false, Uncertain},
		{"partial", 200, `{"ok":false,"error":"fatal_error"}`, false, Uncertain},
		{"unknown", 200, `{"ok":false,"error":"new_error"}`, false, Uncertain},
		{"truncated", 200, `{"ok":true,"ts":"123.456","response_metadata":{"warnings":["message_truncated"]}}`, false, Uncertain},
		{"revoked", 200, `{"ok":false,"error":"token_revoked"}`, false, Rejected},
		{"throttled", 429, `{}`, false, RateLimited},
		{"json-rate-limited", 200, `{"ok":false,"error":"rate_limited"}`, false, RateLimited},
		{"json-ratelimited", 200, `{"ok":false,"error":"ratelimited"}`, false, RateLimited},
		{"server-error", 503, `{}`, false, Uncertain},
		{"malformed", 200, `not-json`, false, Uncertain},
		{"duplicate-fields", 200, `{"ok":false,"ok":true,"ts":"123.456"}`, false, Uncertain},
		{"transport", 0, ``, true, Uncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installation := uuid.NewV7()
			calls := 0
			payload := []byte("{\n\"channel\":\"C1\",\"text\":\"literal\"}")
			client := NewWebClient(credentialsFunc(func(_ context.Context, id uuid.UUID, ref int64) (string, error) {
				if id != installation || ref != 1 {
					t.Fatal("credential scope changed")
				}
				return "fixture-token", nil
			}), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				actual, err := io.ReadAll(r.Body)
				if err != nil || string(actual) != string(payload) || r.GetBody != nil || r.URL.String() != "https://slack.com/api/chat.postMessage" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Fatal("request changed")
				}
				if tc.transportError {
					return nil, errors.New("connection lost")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{"7"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			result := client.Call(t.Context(), installation, 1, "chat.postMessage", payload)
			if calls != 1 || result.Disposition != tc.want {
				t.Fatalf("calls=%d result=%+v", calls, result)
			}
			if tc.want == RateLimited && result.RetryAfter != 7*time.Second {
				t.Fatalf("server retry interval lost: %+v", result)
			}
		})
	}
}

func TestWebClientUsesAppStatusAcknowledgement(t *testing.T) {
	for _, own := range []string{"active", "processing", ""} {
		client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"status":"processing","agent_status":"` + own + `"}`))}, nil
		}))
		result := client.Call(t.Context(), uuid.NewV7(), 1, "agents.sessions.setStatus", []byte(`{"channel_id":"C1","thread_ts":"123.456","status":"active"}`))
		want := Uncertain
		if own == "active" {
			want = Acknowledged
		}
		if result.Disposition != want {
			t.Fatalf("own=%q: %+v", own, result)
		}
	}
}

func TestWebClientStatusSubscriptionWarningRequiresExactAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		want               Disposition
	}{
		{"status-confirmed", "agents.sessions.setStatus", `{"ok":true,"agent_status":"active","response_metadata":{"warnings":["missing_agent_session_stopped_event_subscription"]}}`, Acknowledged},
		{"status-mismatch", "agents.sessions.setStatus", `{"ok":true,"agent_status":"processing","response_metadata":{"warnings":["missing_agent_session_stopped_event_subscription"]}}`, Uncertain},
		{"status-missing", "agents.sessions.setStatus", `{"ok":true,"response_metadata":{"warnings":["missing_agent_session_stopped_event_subscription"]}}`, Uncertain},
		{"mixed-warnings", "agents.sessions.setStatus", `{"ok":true,"agent_status":"active","response_metadata":{"warnings":["missing_agent_session_stopped_event_subscription","unknown_warning"]}}`, Uncertain},
		{"content-warning", "chat.startStream", `{"ok":true,"ts":"123.456","response_metadata":{"warnings":["missing_agent_session_stopped_event_subscription"]}}`, Uncertain},
		{"rejected", "agents.sessions.setStatus", `{"ok":false,"error":"missing_scope","agent_status":"active","response_metadata":{"warnings":["missing_agent_session_stopped_event_subscription"]}}`, Rejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			result := client.Call(t.Context(), uuid.NewV7(), 1, tc.method, []byte(`{"channel_id":"C1","thread_ts":"123.456","status":"active"}`))
			if calls != 1 || result.Disposition != tc.want {
				t.Fatalf("calls=%d result=%+v", calls, result)
			}
		})
	}
}

func TestWebClientNeverFollowsRedirectWithCredentials(t *testing.T) {
	calls := 0
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://elsewhere.invalid/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	result := client.Call(t.Context(), uuid.NewV7(), 1, "chat.postMessage", []byte(`{}`))
	if calls != 1 || result.Disposition != Uncertain {
		t.Fatalf("redirect followed: %d %+v", calls, result)
	}
}

func TestWebClientRequiresExactModalAcknowledgementWithoutReplay(t *testing.T) {
	for _, method := range []string{"views.open", "views.push", "views.update"} {
		for _, tc := range []struct {
			body string
			want Disposition
			code string
		}{
			{`{"ok":true,"view":{"id":"V1"}}`, Acknowledged, ""},
			{`{"ok":true}`, Uncertain, "missing_view_identity"},
			{`{"ok":false,"error":"expired_trigger_id"}`, Rejected, "expired_trigger_id"},
			{`{"ok":false,"error":"exchanged_trigger_id"}`, Rejected, "exchanged_trigger_id"},
			{`{"ok":false,"error":"fatal_error"}`, Uncertain, "slack_outcome_unknown"},
		} {
			count := 0
			client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				count++
				if r.URL.Path != "/api/"+method || r.GetBody != nil {
					t.Fatal("wrong method or replayable request")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			result := client.Call(t.Context(), uuid.NewV7(), 1, method, []byte(`{"trigger_id":"trigger","view":{"type":"modal"}}`))
			if result.Disposition != tc.want || result.Code != tc.code || count != 1 {
				t.Fatalf("modal acknowledgement %+v calls=%d", result, count)
			}
		}
	}
}

func TestWebClientEphemeralUsesPrivateMessageTimestamp(t *testing.T) {
	for _, tc := range []struct {
		body string
		want Disposition
	}{
		{`{"ok":true,"message_ts":"123.456"}`, Acknowledged},
		{`{"ok":true,"ts":"123.456"}`, Uncertain},
		{`{"ok":true}`, Uncertain},
	} {
		client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/chat.postEphemeral" {
				t.Fatal("not private")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		}))
		if got := client.Call(t.Context(), uuid.NewV7(), 1, "chat.postEphemeral", []byte(`{"channel":"C1","user":"U1","text":"hint"}`)); got.Disposition != tc.want {
			t.Fatalf("%s: %+v", tc.body, got)
		}
	}
}
