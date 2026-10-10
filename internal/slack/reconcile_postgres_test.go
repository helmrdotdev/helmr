package slack

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func observedClaim(t *testing.T, claim PostClaim) observedMessage {
	t.Helper()
	var m observedMessage
	if err := json.Unmarshal(claim.Payload, &m); err != nil {
		t.Fatal(err)
	}
	m.Timestamp = "123.789"
	m.AppID = "app"
	m.User = "bot"
	return m
}

func TestSlackPositiveReconciliationRequiresAuthorDestinationAndCompleteContent(t *testing.T) {
	for _, kind := range []string{"valid", "human", "other-app", "channel", "thread", "truncated", "changed-metadata"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.post(t, 1, "content", "lifecycle", nil)
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
				t.Fatal(err)
			}
			m := observedClaim(t, claim)
			switch kind {
			case "human":
				m.User = "human"
			case "other-app":
				m.AppID = "other"
			case "channel":
				m.Channel = "C2"
			case "thread":
				m.Thread = "456.123"
			case "truncated":
				m.Text = "hel"
			case "changed-metadata":
				var data map[string]any
				json.Unmarshal(m.Metadata, &data)
				data["event_payload"].(map[string]any)["revision"] = 2
				m.Metadata, _ = json.Marshal(data)
			}
			ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m)
			if err != nil || ok != (kind == "valid") {
				t.Fatalf("confirmation %s: %v %v", kind, ok, err)
			}
			var state string
			if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM slack_posts WHERE id=$1`, post).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if (state == "posted") != (kind == "valid") {
				t.Fatalf("wrong evidence changed disposition: %s", state)
			}
		})
	}
}

func TestSlackPositiveOpeningReconciliationUnblocksExactThread(t *testing.T) {
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
	post := f.opening(t)
	reply := f.post(t, 2, "reply", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, reply)
	m := observedClaim(t, claim)
	m.Thread = m.Timestamp
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || !ok {
		t.Fatalf("positive opening: %v %v", ok, err)
	}
	next := f.postClaim(t, reply)
	var actual observedMessage
	json.Unmarshal(next.Payload, &actual)
	if actual.Thread != m.Timestamp {
		t.Fatalf("wrong recovered root: %s", next.Payload)
	}
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || ok {
		t.Fatalf("duplicate confirmation changed state: %v %v", ok, err)
	}
}

func TestSlackPositiveUpdateReconciliationRequiresExactMessageTimestamp(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "card", "lifecycle", nil)
	first := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='pending',desired_revision=2 WHERE id=$1`, post)
	f.due(t)
	update := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, update, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	message := observedClaim(t, update)
	message.Timestamp = "987.123"
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, message); err != nil || ok {
		t.Fatalf("different message settled update: %v %v", ok, err)
	}
	message.Timestamp = "123.789"
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, message); err != nil || !ok {
		t.Fatalf("exact update not confirmed: %v %v", ok, err)
	}
}

func TestSlackMessageProofPreservesBlocksAndControls(t *testing.T) {
	expected := []byte(`[{"type":"actions","elements":[{"type":"button","action_id":"answer","value":"exact-control","text":{"type":"plain_text","text":"Answer"}}]}]`)
	generated := []byte(`[{"block_id":"slack-generated","type":"actions","elements":[{"type":"button","action_id":"answer","value":"exact-control","text":{"type":"plain_text","text":"Answer"}}]}]`)
	if !sameMessageBlocks(expected, generated) {
		t.Fatal("generated block identifier hid exact proof")
	}
	for _, replacement := range []struct{ old, new string }{{"exact-control", "different-control"}, {"Answer", "Changed"}, {"answer", "different-action"}} {
		changed := bytes.ReplaceAll(generated, []byte(replacement.old), []byte(replacement.new))
		if sameMessageBlocks(expected, changed) {
			t.Fatal("altered control/content accepted")
		}
	}
	authored := bytes.ReplaceAll(generated, []byte("slack-generated"), []byte("authored-id"))
	if sameMessageBlocks(authored, generated) {
		t.Fatal("authored block identifier ignored")
	}
}

func TestSlackMessageProofTreatsOnlyEmptyBlockRepresentationsAsAbsent(t *testing.T) {
	for _, empty := range [][]byte{nil, []byte(`null`), []byte(`[]`)} {
		if !sameMessageBlocks(nil, empty) {
			t.Fatal("absent blocks were not equivalent")
		}
	}
	if sameMessageBlocks(nil, []byte(`[{"type":"divider"}]`)) {
		t.Fatal("extra rendered content ignored")
	}
	if sameMessageBlocks([]byte(`[{"type":"divider"}]`), []byte(`[{"type":"section","type":"divider"}]`)) {
		t.Fatal("duplicate block keys accepted")
	}
}
