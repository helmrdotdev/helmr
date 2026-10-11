package slack

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (f statusFixture) outputWriter(t *testing.T) func(string) int64 {
	t.Helper()
	receipt, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "projection", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	if _, err = agent.Dispatch(t.Context(), f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	return func(value string) int64 {
		t.Helper()
		raw, _ := json.Marshal(value)
		seq, err := agent.RuntimeOutput(t.Context(), f.Pool, host, execution, receipt.TurnID, uuid.NewV7(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
}
func (f statusFixture) projectAll(t *testing.T) {
	t.Helper()
	for range 100 {
		progressed, err := projectParticipant(t.Context(), f.Pool, f.participant, ProjectionConfig{PublicURL: &url.URL{Scheme: "https", Host: "console.example.test"}, ControlKey: bytes.Repeat([]byte{4}, 32)})
		if err != nil {
			t.Fatal(err)
		}
		if !progressed {
			return
		}
	}
	t.Fatal("projection did not reach tail")
}
func (f statusFixture) firstPost(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_posts WHERE thread_source_id=$1 ORDER BY seq LIMIT 1`, f.participant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestSlackProjectionAuthorizationCutoffDoesNotReintroduceCumulativeBytes(t *testing.T) {
	for _, path := range []string{"acknowledgment", "reconciliation"} {
		t.Run(path, func(t *testing.T) {
			f, store, org := credentialFixture(t)
			write := f.outputWriter(t)
			write("visible-prefix")
			f.projectAll(t)
			old := f.firstPost(t)
			claim := f.postClaim(t, old)
			write("SUPPRESSED-DESIRED")
			f.projectAll(t)
			if path == "reconciliation" {
				if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
					t.Fatal(err)
				}
			}
			write("SUPPRESSED-UNPROJECTED")
			if _, err := f.reauthorize(t, store, org, refreshResult().grant); err != nil {
				t.Fatal(err)
			}
			write("new-window")
			f.projectAll(t)
			if path == "reconciliation" {
				if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, claim)); err != nil || !ok {
					t.Fatal(ok, err)
				}
			} else if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
				t.Fatal(err)
			}
			var oldClosed bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT closed_at IS NOT NULL AND status='suppressed' AND desired_revision=2 AND confirmed_revision=1 FROM slack_posts WHERE id=$1`, old).Scan(&oldClosed); err != nil || !oldClosed {
				t.Fatal("old publication reopened", err)
			}
			var body []byte
			var count int
			if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE thread_source_id=$1 AND id<>$2`, f.participant, old).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(body, []byte("new-window")) || bytes.Contains(body, []byte("SUPPRESSED")) || bytes.Contains(body, []byte("visible-prefix")) {
				t.Fatalf("cross-window body: %s", body)
			}
			f.projectAll(t)
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_source_id=$1`, f.participant).Scan(&count); err != nil || count != 2 {
				t.Fatal("replayed intents", count, err)
			}
		})
	}
}
func TestSlackProjectionUsesInstallationIdentityAcrossContinuations(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agents SET name='first' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Agent)
	write("first")
	f.projectAll(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agents SET name='second' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Agent)
	write(strings.Repeat("😀", 4000))
	f.projectAll(t)
	rows, err := f.Pool.Query(t.Context(), `SELECT payload,continuation_ordinal FROM slack_posts WHERE thread_source_id=$1 ORDER BY seq`, f.participant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var body []byte
		var ordinal int
		if err := rows.Scan(&body, &ordinal); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(body, &p); err != nil || p["username"] != nil || p["icon_url"] != nil || p["icon_emoji"] != nil || ordinal != count {
			t.Fatal(string(body), err)
		}
		count++
	}
	if err := rows.Err(); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestSlackProjectionWorkerResumesOrderedCursorWithoutDuplicateIntents(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	write("one")
	write(" two")
	config := ProjectionConfig{PublicURL: &url.URL{Scheme: "https", Host: "console.example.test"}, ControlKey: bytes.Repeat([]byte{4}, 32)}
	for range 100 {
		n, err := ReconcileProjection(t.Context(), f.Pool, config, 64)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	var count int
	var cursor, next int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT p.projected_event_seq,s.next_event_seq,(SELECT count(*) FROM slack_posts WHERE thread_source_id=p.id) FROM slack_thread_sources p JOIN sessions s ON s.id=p.session_id WHERE p.id=$1`, f.participant).Scan(&cursor, &next, &count); err != nil {
		t.Fatal(err)
	}
	if cursor != next-1 || count != 1 {
		t.Fatalf("cursor=%d next=%d posts=%d", cursor, next, count)
	}
	for range 3 {
		if n, err := ReconcileProjection(t.Context(), f.Pool, config, 64); err != nil || n != 0 {
			t.Fatal("restarted projection duplicated", n, err)
		}
	}
	write(" three")
	f.projectAll(t)
	var body []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE thread_source_id=$1`, f.participant).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("one two three")) {
		t.Fatal("resumed output missing", string(body))
	}
}

func TestSlackProjectionQuestionBypassesUnknownOutputAndWithdrawsAnswerControl(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	write("unknown output")
	f.projectAll(t)
	output := f.firstPost(t)
	unknown := f.postClaim(t, output)
	if err := FinishPost(t.Context(), f.Pool, unknown, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	var turn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	ask := uuid.NewV7()
	question := json.RawMessage(`{"prompt":[{"type":"text","text":"Choose a path"}],"answer":{"type":"choice","options":[{"id":"a","label":"Display label","description":"Why this choice","value":"PRIVATE_MACHINE_VALUE"}]}}`)
	if err := agent.RuntimeAsk(t.Context(), f.Pool, host, execution, turn, ask, question); err != nil {
		t.Fatal(err)
	}
	f.projectAll(t)
	var post uuid.UUID
	var body []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT id,payload FROM slack_posts WHERE ask_id=$1`, ask).Scan(&post, &body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("Display label")) || !bytes.Contains(body, []byte("Why this choice")) || bytes.Contains(body, []byte("PRIVATE_MACHINE_VALUE")) || !bytes.Contains(body, []byte("helmr.open_answer")) {
		t.Fatal("question presentation", string(body))
	}
	f.due(t)
	q := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, q, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.791"}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.RespondAsk(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, AskID: ask, ResponseID: "answer", Answer: json.RawMessage(`{"selected":[{"id":"a","value":"PRIVATE_MACHINE_VALUE"}]}`)}); err != nil {
		t.Fatal(err)
	}
	f.projectAll(t)
	f.due(t)
	updated := f.postClaim(t, post)
	if updated.Method != "chat.update" || bytes.Contains(updated.Payload, []byte("helmr.open_answer")) || bytes.Contains(updated.Payload, []byte("helmr_answer_cli")) || bytes.Contains(updated.Payload, []byte("PRIVATE_MACHINE_VALUE")) || !bytes.Contains(updated.Payload, []byte("Question answered.")) {
		t.Fatal("answered update", string(updated.Payload))
	}
	f.noPostClaim(t, output)
}
