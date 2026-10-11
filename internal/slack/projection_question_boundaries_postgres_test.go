package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestSlackQuestionPromptBoundariesRemainVisibleAndAdvance(t *testing.T) {
	var parts []map[string]string
	for i := 0; i < 64; i++ {
		parts = append(parts, map[string]string{"type": "text", "text": fmt.Sprintf("part-%02d-%s", i, strings.Repeat("x", 64))})
	}
	full, _ := json.Marshal(map[string]any{"prompt": parts, "answer": map[string]any{"type": "choice", "options": []map[string]string{{"id": "a", "label": "Complete choice label", "description": "Complete choice description", "value": "PRIVATE_VALUE"}}}})
	for _, tc := range []struct {
		name     string
		question json.RawMessage
		answer   bool
	}{
		{"full-choice", full, true},
		{"long-description", json.RawMessage(`{"prompt":[{"type":"text","text":"Full prompt"}],"answer":{"type":"choice","options":[{"id":"a","label":"Choice","description":"` + strings.Repeat("d", 76) + `","value":"PRIVATE_VALUE"}]}}`), false},
		{"long-label", json.RawMessage(`{"prompt":[{"type":"text","text":"Full prompt"}],"answer":{"type":"choice","options":[{"id":"a","label":"` + strings.Repeat("l", 76) + `","value":"PRIVATE_VALUE"}]}}`), false},
		{"large-prompt", json.RawMessage(`{"prompt":[{"type":"text","text":"` + strings.Repeat("x", 245000) + `"}],"answer":{"type":"text"}}`), false},
		{"empty-array-text", json.RawMessage(`{"prompt":[],"answer":{"type":"text"}}`), true},
		{"empty-part-text", json.RawMessage(`{"prompt":[{"type":"text","text":""}],"answer":{"type":"text"}}`), true},
	} {
		for _, unbound := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unbound=%v", tc.name, unbound), func(t *testing.T) {
				f := newStatusFixture(t)
				var project uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT project_id FROM environments WHERE id=$1`, f.Environment).Scan(&project); err != nil {
					t.Fatal(err)
				}
				if unbound {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
				}
				write := f.outputWriter(t)
				var turn uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
					t.Fatal(err)
				}
				execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
				host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
				ask := uuid.NewV7()
				if err := agent.RuntimeAsk(t.Context(), f.Pool, host, execution, turn, ask, tc.question); err != nil {
					t.Fatal(err)
				}
				last := write("after-question")
				f.projectAll(t)
				f.projectAll(t)
				rows, err := f.Pool.Query(t.Context(), `SELECT payload,count(*) OVER () FROM slack_posts WHERE ask_id=$1 ORDER BY continuation_ordinal`, ask)
				if err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				count := 0
				for rows.Next() {
					var raw []byte
					var total int
					if err := rows.Scan(&raw, &total); err != nil {
						t.Fatal(err)
					}
					var body MessageBody
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Fatal(err)
					}
					for _, block := range body.Blocks {
						if block.Type == "rich_text" {
							for _, element := range block.Elements {
								for _, part := range element.Elements {
									text.WriteString(part.Text)
								}
							}
						}
					}
					lastPost := count == total-1
					if bytes.Contains(raw, []byte("helmr.open_answer")) != (tc.answer && lastPost) || bytes.Contains(raw, []byte("PRIVATE_VALUE")) {
						t.Fatal("question controls or private data", string(raw))
					}
					var card struct {
						Blocks []struct {
							ID   string `json:"block_id"`
							Text struct {
								Text string `json:"text"`
							} `json:"text"`
						} `json:"blocks"`
					}
					if err := json.Unmarshal(raw, &card); err != nil {
						t.Fatal(err)
					}
					guide := ""
					for _, block := range card.Blocks {
						if block.ID == "helmr_answer_cli" {
							guide = block.Text.Text
						}
					}
					if (guide != "") != lastPost {
						t.Fatalf("CLI guidance must appear only on the last question post: post %d of %d", count+1, total)
					}
					if lastPost {
						target := fmt.Sprintf("%s %s %s --project %s --env %s", f.Session, turn, ask, project, f.Environment)
						for _, expected := range []string{"session turn ask get " + target + " --json", "session turn ask respond " + target + " --answer-file answer.json --response-id RESPONSE_ID", "helmr login", "--api-url", "unset HELMR_API_KEY", "same answer for retries"} {
							if !strings.Contains(guide, expected) {
								t.Fatalf("missing CLI guidance %q: %s", expected, guide)
							}
						}
					}
					count++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				rows.Close()
				if count == 0 {
					t.Fatal("question has no publication")
				}
				if tc.name == "full-choice" {
					for _, part := range parts {
						if !strings.Contains(text.String(), part["text"]) {
							t.Fatal("missing prompt part", part["text"])
						}
					}
					if !strings.Contains(text.String(), "Complete choice label") || !strings.Contains(text.String(), "Complete choice description") {
						t.Fatal("missing choice decoration")
					}
				} else if tc.name == "long-description" && !strings.Contains(text.String(), strings.Repeat("d", 76)) {
					t.Fatal("choice description truncated")
				} else if tc.name == "long-label" && !strings.Contains(text.String(), strings.Repeat("l", 76)) {
					t.Fatal("choice label truncated")
				} else if tc.name == "large-prompt" && text.String() != strings.Repeat("x", 245000) {
					t.Fatal("prompt truncated")
				} else if strings.HasPrefix(tc.name, "empty-") && !strings.Contains(text.String(), "Question awaiting your answer.") {
					t.Fatal("missing empty question label", text.String())
				}
				var cursor int64
				var later int
				if err := f.Pool.QueryRow(t.Context(), `SELECT projected_event_seq FROM slack_thread_sources WHERE id=$1`, f.participant).Scan(&cursor); err != nil || cursor != last {
					t.Fatal("cursor stalled", cursor, last, err)
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_source_id=$1 AND role='intermediate' AND convert_from(payload,'UTF8')::jsonb->>'text' LIKE '%after-question%'`, f.participant).Scan(&later); err != nil || later != 1 {
					t.Fatal("later output", later, err)
				}
				first := f.firstPost(t)
				if unbound {
					f.noPostClaim(t, first)
					// Only the admitted opening can establish a physical root; a question waits.
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts='123.456' WHERE id=$1`, f.thread)
				}
				claim := f.postClaim(t, first)
				var payload map[string]any
				if err := json.Unmarshal(claim.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["thread_ts"] != "123.456" {
					t.Fatal("question wrong thread", payload)
				}
			})
		}
	}
}
