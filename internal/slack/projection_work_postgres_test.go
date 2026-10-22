package slack

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/session"
	"strings"
	"testing"
	"uuid"
)

func TestSlackWorkBoundPreservesOriginalUnicodeAndLeavesOneConsoleNotice(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	original := strings.Repeat("説明😀\n", 3000)
	write(original)
	f.projectAll(t)
	write("additional text remains in Console")
	f.projectAll(t)
	rows, err := f.Pool.Query(t.Context(), `SELECT payload,post_key FROM slack_posts WHERE thread_source_id=$1 AND role='intermediate' ORDER BY seq`, f.participant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var text strings.Builder
	count, notices := 0, 0
	for rows.Next() {
		var raw []byte
		var key string
		if err = rows.Scan(&raw, &key); err != nil {
			t.Fatal(err)
		}
		count++
		if strings.HasPrefix(key, "overflow:") {
			notices++
			if !strings.Contains(string(raw), "Console") {
				t.Fatal("missing escape")
			}
			continue
		}
		var body MessageBody
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		var part strings.Builder
		for _, b := range body.Blocks {
			for _, e := range b.Elements {
				for _, p := range e.Elements {
					part.WriteString(p.Text)
				}
			}
		}
		if part.Len() > 6000 || len(raw) > 65536 {
			t.Fatal("unbounded work message", part.Len(), len(raw))
		}
		text.WriteString(part.String())
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 4 || notices != 1 || text.Len() == 0 || !strings.HasPrefix(original, text.String()) {
		t.Fatal("original text or cap changed", count, notices, text.Len())
	}
}

func TestSlackWorkCapIncludesQuestionSeparatedGroups(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	var turn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		write(strings.Repeat("x", i+1))
		f.projectAll(t)
		ask := uuid.NewV7()
		if err := session.RuntimeAsk(t.Context(), f.Pool, activityHost(f), activityExecution(f), turn, ask, json.RawMessage(`{"type":"text","prompt":[{"type":"text","text":"Continue?"}]}`)); err != nil {
			t.Fatal(err)
		}
		f.projectAll(t)
		if _, err := session.AnswerAsk(t.Context(), f.Pool, session.Caller{Kind: "user", ID: f.User}, session.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, AskID: ask, AnswerID: uuid.NewV7().String(), Answer: json.RawMessage(`"yes"`)}); err != nil {
			t.Fatal(err)
		}
		f.projectAll(t)
	}
	rows, err := f.Pool.Query(t.Context(), `SELECT role FROM slack_posts WHERE thread_source_id=$1 AND role<>'controls' ORDER BY seq`, f.participant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	roles := []string{}
	for rows.Next() {
		var role string
		if err = rows.Scan(&role); err != nil {
			t.Fatal(err)
		}
		roles = append(roles, role)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := "intermediate,question,intermediate,question,intermediate,question,intermediate,question,question,question"
	if strings.Join(roles, ",") != want {
		t.Fatalf("ask boundary or cap lost: %v", roles)
	}
}
