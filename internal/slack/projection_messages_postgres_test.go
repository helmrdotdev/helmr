package slack

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestSlackSteeringFailureKeepsExactDispositionWithoutRequeue(t *testing.T) {
	for _, kind := range []string{"not-started", "started-then-stopped", "callback-failed", "explicit-rejection", "delivered"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			f.outputWriter(t)
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			if err := agent.RuntimeRegisterMessages(t.Context(), f.Pool, host, execution, turn); err != nil {
				t.Fatal(err)
			}
			request := f.reply(t, "steer", false)
			if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), request); err != nil {
				t.Fatal(err)
			}
			var message uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT message_id FROM slack_requests WHERE id=$1 AND status='accepted' AND operation='send' AND turn_id=$2`, request, turn).Scan(&message); err != nil {
				t.Fatal(err)
			}
			attachment, err := agent.AcquireRuntimeAttachment(t.Context(), f.Pool, host, execution)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "not-started" {
				m, err := agent.ClaimRuntimeMessage(t.Context(), f.Pool, host, execution, attachment.Sequence, turn)
				if err != nil || m == nil || m.ID != message {
					t.Fatal(m, err)
				}
			}
			if kind == "not-started" || kind == "started-then-stopped" {
				if _, err = agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "interrupt", RetryKey: "stop"}); err != nil {
					t.Fatal(err)
				}
			} else {
				reason := map[string]string{"callback-failed": "callback_failed", "explicit-rejection": "message_rejected", "delivered": ""}[kind]
				if err = agent.CompleteRuntimeMessage(t.Context(), f.Pool, host, execution, attachment.Sequence, turn, message, reason); err != nil {
					t.Fatal(err)
				}
			}
			f.projectAll(t)
			f.projectAll(t)
			want := "not_delivered"
			text := "Message not delivered."
			if kind == "started-then-stopped" || kind == "callback-failed" {
				want = "uncertain"
				text = "Message delivery uncertain."
			}
			if kind == "delivered" {
				want = "delivered"
			}
			var exact bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_events WHERE session_id=$1 AND kind IN ('message.rejected','message.delivered') AND convert_from(data,'UTF8')::jsonb->>'delivery'=$2) AND (SELECT count(*) FROM turns WHERE session_id=$1)=1`, f.Session, want).Scan(&exact); err != nil || !exact {
				t.Fatal(exact, err)
			}
			var posts int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE request_id=$1`, request).Scan(&posts); err != nil {
				t.Fatal(err)
			}
			if kind == "delivered" {
				if posts != 0 {
					t.Fatal("success generated public receipt")
				}
				return
			}
			if posts != 1 {
				t.Fatal("failure feedback duplicated", posts)
			}
			var payload []byte
			if err = f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE request_id=$1 AND role='request_feedback'`, request).Scan(&payload); err != nil || !bytes.Contains(payload, []byte(text)) {
				t.Fatal(string(payload), err)
			}
			if err = admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), request); err != nil {
				t.Fatal(err)
			}
			if err = f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM turns WHERE session_id=$1)=1 AND message_id=$3 AND status='accepted' AND operation='send' FROM slack_requests WHERE id=$2`, f.Session, request, message).Scan(&exact); err != nil || !exact {
				t.Fatal("late failure replayed input", exact, err)
			}
		})
	}
}
