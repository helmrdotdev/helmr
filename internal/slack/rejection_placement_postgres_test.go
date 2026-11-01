package slack

import (
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackRejectionFeedbackAnswersWhereTheInputWasMade(t *testing.T) {
	for _, scenario := range []struct {
		name, code, method, thread, user string
		input                            func(statusFixture, *testing.T) (uuid.UUID, string)
	}{
		{name: "session-wide thread reply", code: "execution_capacity_exhausted", method: "chat.postMessage", thread: "123.456",
			input: func(f statusFixture, t *testing.T) (uuid.UUID, string) { return f.reply(t, "capacity", false), "" }},
		{name: "session-wide new mention", code: "execution_limit", method: "chat.postMessage", thread: "message",
			input: func(f statusFixture, t *testing.T) (uuid.UUID, string) { return f.message(t, "C1") }},
		{name: "personal thread reply", code: "content_unsupported", method: "chat.postEphemeral", thread: "123.456", user: "human",
			input: func(f statusFixture, t *testing.T) (uuid.UUID, string) { return f.reply(t, "content", true), "" }},
		{name: "personal new mention", code: "content_unsupported", method: "chat.postEphemeral", user: "human",
			input: func(f statusFixture, t *testing.T) (uuid.UUID, string) { return f.message(t, "C1") }},
		{name: "direct message", code: "content_unsupported", method: "chat.postMessage", thread: "message",
			input: func(f statusFixture, t *testing.T) (uuid.UUID, string) { f.grantDirect(t); return f.message(t, "D1") }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newStatusFixture(t)
			id, message := scenario.input(f, t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error=$2 WHERE id=$1`, id, scenario.code)
			intent, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
			if err != nil || !found || intent == nil {
				t.Fatalf("feedback missing: %v %v", found, err)
			}
			var body struct {
				Thread string `json:"thread_ts"`
				User   string `json:"user"`
			}
			if err = json.Unmarshal(intent.payload, &body); err != nil {
				t.Fatal(err)
			}
			want := scenario.thread
			if want == "message" {
				want = message
			}
			if intent.method != scenario.method || body.Thread != want || body.User != scenario.user {
				t.Fatalf("placement: %s %s", intent.method, intent.payload)
			}
		})
	}
}

// message records a top-level message receipt and returns its timestamp.
func (f statusFixture) message(t *testing.T, channel string) (uuid.UUID, string) {
	t.Helper()
	ts := slackNow()
	payload, _ := json.Marshal(messageGesture{Channel: channel, Timestamp: ts, Text: "<@bot> new task"})
	now := time.Now().UTC()
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "message:" + ts, Actor: "human", OccurredAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID, ts
}
