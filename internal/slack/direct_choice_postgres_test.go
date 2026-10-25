package slack

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (f statusFixture) choiceAsk(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	turn, _ := f.pendingQuestion(t)
	ask := uuid.NewV7()
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
	h := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	q := json.RawMessage(`{"prompt":[{"type":"text","text":"Include mobile?"}],"type":"choice","allowNotes":true,"options":[{"id":"yes","label":"Include mobile","value":{"private":true}},{"id":"no","label":"Web only","value":{"private":false}}]}`)
	if err := session.RuntimeAsk(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, h, e, turn, ask, q); err != nil {
		t.Fatal(err)
	}
	return turn, ask
}
func TestDirectChoiceUsesVerifiedActorAndExactQuestion(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlinked", true: "operator"}[linked], func(t *testing.T) {
			f := newStatusFixture(t)
			if linked {
				f.link(t)

			}
			turn, ask := f.choiceAsk(t)
			if linked {
				f.SetPermissions(t, "agents.use", "sessions.send")
			}
			c := f.control("choose")
			c.Target.Turn, c.Target.Ask, c.OptionID = turn, ask, "yes"
			key := []byte(strings.Repeat("k", 32))
			signed, err := encodeControl(key, c)
			if err != nil {
				t.Fatal(err)
			}
			var channel, root string
			if err = f.Pool.QueryRow(t.Context(), `SELECT slack_channel_id,thread_ts FROM slack_threads WHERE id=$1`, f.thread).Scan(&channel, &root); err != nil {
				t.Fatal(err)
			}
			h := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: key}
			stamp := slackNow()
			payload := func(ch string) []byte {
				return formCallbackBody(t, map[string]any{"type": "block_actions", "channel": map[string]string{"id": ch}, "message": map[string]string{"ts": "123.790", "thread_ts": root}, "actions": []any{map[string]string{"type": "button", "action_id": "helmr.choose.0", "value": signed, "action_ts": stamp}}})
			}
			if r := serveSignedInteraction(h, payload("COTHER")); r.Code != http.StatusBadRequest {
				t.Fatalf("cross-thread %d", r.Code)
			}
			for range 2 {
				if r := serveSignedInteraction(h, payload(channel)); r.Code != http.StatusOK {
					t.Fatalf("ingress %d %s", r.Code, r.Body.String())
				}
			}
			requestKey, err := controlRequestKey(c, "human", stamp)
			if err != nil {
				t.Fatal(err)
			}
			var receipt uuid.UUID
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests WHERE request_key=$1`, requestKey).Scan(&receipt); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() {
					if err := admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, receipt); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			var state, code string
			if err = f.Pool.QueryRow(t.Context(), `SELECT status,COALESCE(error,'') FROM slack_requests WHERE id=$1`, receipt).Scan(&state, &code); err != nil {
				t.Fatal(err)
			}
			if !linked {
				if state != "rejected" || code != "identity_unlinked" {
					t.Fatal(state, code)
				}
				return
			}
			if state != "accepted" {
				t.Fatal(state, code)
			}
			var answer []byte
			var actor uuid.UUID
			if err = f.Pool.QueryRow(t.Context(), `SELECT answer,answered_by_user_id FROM turn_asks WHERE id=$1`, ask).Scan(&answer, &actor); err != nil || actor != f.User || string(answer) != `{"selected":[{"id":"yes","value":{"private":true}}]}` {
				t.Fatal(string(answer), actor, err)
			}
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE kind='ask.answered' AND convert_from(data,'UTF8')::jsonb->>'ask_id'=$1`, ask.String()).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			f.projectAll(t)
			var body []byte
			if err = f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE ask_id=$1 ORDER BY seq DESC LIMIT 1`, ask).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "helmr.choose.") || strings.Contains(string(body), "private") || !strings.Contains(string(body), "Answered by") || !strings.Contains(string(body), "Include mobile") {
				t.Fatal(string(body))
			}
			// A different gesture is not a transport retry and cannot override the answer.
			c.Nonce = uuid.NewV7()
			c.OptionID = "no"
			later := f.controlReceipt(t, c, nil)
			if err = admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, later); err != nil {
				t.Fatal(err)
			}
			if err = f.Pool.QueryRow(t.Context(), `SELECT error FROM slack_requests WHERE id=$1`, later).Scan(&code); err != nil || code != "ask_already_answered" {
				t.Fatal(code, err)
			}
		})
	}
}
func TestDirectChoicePresentationKeepsCompletePublicMeaning(t *testing.T) {
	f := newStatusFixture(t)
	turn, ask := f.choiceAsk(t)
	var question []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT question FROM turn_asks WHERE id=$1`, ask).Scan(&question); err != nil {
		t.Fatal(err)
	}
	o := projectionOwner{installation: f.installation, thread: f.thread, participant: f.participant, environment: f.Environment, session: f.Session, front: f.Session, config: testProjectionConfig(), project: "project", environmentRef: "dev"}
	blocks, err := presentationBlocks(o, projectionEvent{turn: &turn, at: time.Now()}, "question", &ask, question)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(blocks)
	if strings.Count(string(raw), "helmr.choose.") != 2 || !strings.Contains(string(raw), "helmr.open_answer") || strings.Contains(string(raw), "Helmr Agent") || strings.Contains(string(raw), "private") {
		t.Fatal(string(raw))
	}
}
