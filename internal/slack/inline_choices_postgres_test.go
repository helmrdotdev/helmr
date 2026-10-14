package slack

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"
)

func TestInlineChoicesCommitOnlyOnSubmitWithCurrentHumanAuthority(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlinked", true: "linked"}[linked], func(t *testing.T) {
			f := newStatusFixture(t)
			if linked {
				f.link(t)
			}
			turn, ask := f.choiceAsk(t)
			c := f.control("submit_choices")
			c.Target.Turn, c.Target.Ask = turn, ask
			h := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
			signed, err := encodeControl(h.ControlKey, c)
			if err != nil {
				t.Fatal(err)
			}
			var channel, root string
			if err := f.Pool.QueryRow(t.Context(), `SELECT slack_channel_id,thread_ts FROM slack_threads WHERE id=$1`, f.thread).Scan(&channel, &root); err != nil {
				t.Fatal(err)
			}
			payload := func(actionType, actionID, stamp, ch, selected string) []byte {
				return formCallbackBody(t, map[string]any{
					"type": "block_actions", "channel": map[string]string{"id": ch}, "message": map[string]string{"thread_ts": root},
					"actions": []any{map[string]string{"type": actionType, "action_id": actionID, "value": signed, "action_ts": stamp}},
					"state":   map[string]any{"values": map[string]any{"choice": map[string]any{"choice": map[string]any{"type": "radio_buttons", "selected_option": map[string]string{"value": selected}}}}},
				})
			}
			if r := serveSignedInteraction(h, payload("radio_buttons", "choice", slackNow(), channel, "yes")); r.Code != http.StatusOK {
				t.Fatal(r.Code)
			}
			var receipts int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatal("selection admitted an answer", receipts, err)
			}
			stamp := slackNow()
			if r := serveSignedInteraction(h, payload("button", "helmr.submit_choices", stamp, "COTHER", "yes")); r.Code != http.StatusBadRequest {
				t.Fatal("cross-thread submission", r.Code)
			}
			for range 2 {
				if r := serveSignedInteraction(h, payload("button", "helmr.submit_choices", stamp, channel, "yes")); r.Code != http.StatusOK {
					t.Fatal(r.Code)
				}
			}
			key, err := controlRequestKey(c, "human", stamp)
			if err != nil {
				t.Fatal(err)
			}
			var receipt uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests WHERE request_key=$1`, key).Scan(&receipt); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := admitControl(t.Context(), f.Pool, receipt); err != nil {
					t.Fatal(err)
				}
			}
			var status, code string
			if err := f.Pool.QueryRow(t.Context(), `SELECT status,COALESCE(error,'') FROM slack_requests WHERE id=$1`, receipt).Scan(&status, &code); err != nil {
				t.Fatal(err)
			}
			if !linked {
				if status != "rejected" || code != "identity_unlinked" {
					t.Fatal(status, code)
				}
				return
			}
			var answer []byte
			var actor uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT answer,answered_by_user_id FROM turn_asks WHERE id=$1`, ask).Scan(&answer, &actor); err != nil || status != "accepted" || actor != f.User || string(answer) != `{"selected":[{"id":"yes","value":{"private":true}}]}` {
				t.Fatal(string(answer), actor, status, code, err)
			}
			// Same gesture with different state is a conflict, never an answer edit.
			if r := serveSignedInteraction(h, payload("button", "helmr.submit_choices", stamp, channel, "no")); r.Code != http.StatusConflict {
				t.Fatal("changed replay admitted", r.Code)
			}
			f.projectAll(t)
			var body []byte
			if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE ask_id=$1 ORDER BY seq DESC LIMIT 1`, ask).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "helmr.submit_choices") || strings.Contains(string(body), "radio_buttons") || strings.Contains(string(body), "private") || !strings.Contains(string(body), "Answered by") {
				t.Fatal(string(body))
			}
		})
	}
}

func TestInlineChoicePresentationHasOneVisibleOptionList(t *testing.T) {
	for _, tc := range []struct{ name, fields, options, kind string }{
		{"buttons", ``, `[{"id":"a","label":"First","value":null},{"id":"b","label":"Second","value":null},{"id":"c","label":"Third","value":null},{"id":"d","label":"Fourth","value":null}]`, "button"},
		{"radio", `,"allowCustom":true`, `[{"id":"a","label":"First","description":"Explanation","value":"PRIVATE"}]`, "radio_buttons"},
		{"checkboxes", `,"multiple":true`, `[{"id":"a","label":"First","value":null}]`, "checkboxes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			question := json.RawMessage(`{"type":"choice","prompt":[{"type":"text","text":"Choose a path"}]` + tc.fields + `,"options":` + tc.options + `}`)
			posts, err := renderQuestion(1, question)
			if err != nil || len(posts) != 1 {
				t.Fatal(posts, err)
			}
			prompt, _ := json.Marshal(posts[0].Body.Blocks)
			if strings.Contains(string(prompt), "First") || !strings.Contains(posts[0].Body.Text, "First") {
				t.Fatal("duplicated visible choices or missing notification choices", string(prompt), posts[0].Body.Text)
			}
			id := uuid.NewV7()
			o := projectionOwner{installation: id, thread: id, participant: id, environment: id, session: id, config: testProjectionConfig()}
			blocks, err := presentationBlocks(o, projectionEvent{turn: &id, at: time.Now()}, "question", &id, question)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(blocks)
			if strings.Count(string(raw), "First") != 1 || !strings.Contains(string(raw), tc.kind) || strings.Contains(string(raw), "PRIVATE") {
				t.Fatal(string(raw))
			}
			if tc.kind != "button" && !strings.Contains(string(raw), "helmr.submit_choices") {
				t.Fatal("missing explicit submit", string(raw))
			}
			if tc.name == "radio" && !strings.Contains(string(raw), "Write an answer") {
				t.Fatal("missing custom modal", string(raw))
			}
		})
	}
}

func TestInlineChoiceEmptyLabelUsesCompleteFallback(t *testing.T) {
	question := json.RawMessage(`{"type":"choice","prompt":[{"type":"text","text":"Choose"}],"options":[{"id":"a","label":"","description":"Retained description","value":null}]}`)
	q, err := parseFormQuestion(question)
	if err != nil {
		t.Fatal(err)
	}
	if inlineChoice(q) || shortChoice(q) {
		t.Fatal("empty label admitted inline")
	}
	posts, err := renderQuestion(1, question)
	if err != nil || len(posts) != 1 || !strings.Contains(posts[0].Body.Text, "Retained description") {
		t.Fatal(posts, err)
	}
	id := uuid.NewV7()
	blocks, err := presentationBlocks(projectionOwner{installation: id, thread: id, participant: id, environment: id, session: id, config: testProjectionConfig()}, projectionEvent{turn: &id, at: time.Now()}, "question", &id, question)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(blocks)
	if strings.Contains(string(raw), "actions") || !strings.Contains(string(raw), "View question and CLI instructions") {
		t.Fatal(string(raw))
	}
}
