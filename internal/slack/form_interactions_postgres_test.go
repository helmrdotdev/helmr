package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func formCallbackBody(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	payload["api_app_id"] = "app"
	payload["team"] = map[string]string{"id": "team"}
	if payload["user"] == nil {
		payload["user"] = map[string]string{"id": "human"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(url.Values{"payload": []string{string(raw)}}.Encode())
}

func TestSlackFormOpeningAndSubmissionKeepImmediateReceiptBoundary(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	c := f.control("open_answer")
	c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
	h := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	signed, err := encodeControl(h.ControlKey, c)
	if err != nil {
		t.Fatal(err)
	}
	var metadata string
	opened := slackNow()
	h.Client = callerFunc(func(ctx context.Context, installation uuid.UUID, credential int64, method string, payload []byte) DeliveryResult {
		if installation != f.installation || credential != 1 || method != "views.open" {
			t.Fatal("wrong modal destination")
		}
		// External opening must not retain adapter authorization locks.
		if err := db.RunTx(ctx, f.Pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT id FROM slack_installations WHERE id=$1 FOR UPDATE NOWAIT`, f.installation)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `SELECT id FROM slack_channels WHERE id=$1 FOR UPDATE NOWAIT`, f.channel)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var request struct {
			Trigger string `json:"trigger_id"`
			View    struct {
				Metadata string `json:"private_metadata"`
			} `json:"view"`
		}
		if err := json.Unmarshal(payload, &request); err != nil || request.Trigger != "trigger" {
			t.Fatalf("bad opening: %s %v", payload, err)
		}
		metadata = request.View.Metadata
		claims, err := decodeControl(h.ControlKey, metadata, time.Now())
		if err != nil || claims.Action != "answer" || claims.Actor != "human" || claims.SourceTimestamp != opened || claims.Target != c.Target {
			t.Fatalf("bad form claims: %+v %v", claims, err)
		}
		return DeliveryResult{Disposition: Acknowledged, ViewID: "V1"}
	})
	body := formCallbackBody(t, map[string]any{"type": "block_actions", "trigger_id": "trigger", "actions": []any{map[string]string{"type": "button", "action_id": "helmr.open_answer", "value": signed, "action_ts": opened}}})
	if response := serveSignedInteraction(h, body); response.Code != http.StatusOK || metadata == "" {
		t.Fatalf("open: %d %s", response.Code, response.Body.String())
	}
	submit := func(answer string) []byte {
		return formCallbackBody(t, map[string]any{"type": "view_submission", "view": map[string]any{"callback_id": "helmr.answer", "private_metadata": metadata, "state": map[string]any{"values": map[string]any{"text": map[string]any{"text": map[string]any{"type": "plain_text_input", "value": answer}}}}}})
	}
	for range 2 {
		if response := serveSignedInteraction(h, submit("yes")); response.Code != http.StatusOK {
			t.Fatalf("submit: %d %s", response.Code, response.Body.String())
		}
	}
	var id uuid.UUID
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.id,a.status='pending' FROM slack_requests r CROSS JOIN turn_asks a WHERE a.id=$1 AND r.status='received'`, c.Target.Ask).Scan(&id, &pending); err != nil || !pending {
		t.Fatalf("ACK admitted core answer: %v %v", pending, err)
	}
	if response := serveSignedInteraction(h, submit("changed")); response.Code != http.StatusConflict {
		t.Fatalf("changed form reused receipt: %d", response.Code)
	}
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	if response := serveSignedInteraction(h, submit("yes")); response.Code != http.StatusOK {
		t.Fatalf("accepted retry: %d", response.Code)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT a.status='responded' AND a.answer=$2 AND r.status='accepted' AND r.source_occurred_at=$3 FROM turn_asks a JOIN slack_requests r ON r.ask_id=a.id WHERE a.id=$1`, c.Target.Ask, []byte(`"yes"`), func() time.Time { v, _ := slackMessageTime(opened); return v }()).Scan(&exact); err != nil || !exact {
		t.Fatalf("answer/provenance changed: %v %v", exact, err)
	}
}

func TestSlackDeniedFormCannotReviveAfterIdentityRestoration(t *testing.T) {
	f := newStatusFixture(t)
	turn, ask := f.pendingQuestion(t)
	c := f.control("answer")
	c.Target.Turn, c.Target.Ask = turn, ask
	h := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	signed, err := encodeControl(h.ControlKey, c)
	if err != nil {
		t.Fatal(err)
	}
	body := formCallbackBody(t, map[string]any{"type": "view_submission", "view": map[string]any{"callback_id": "helmr.answer", "private_metadata": signed, "state": map[string]any{"values": map[string]any{"text": map[string]any{"text": map[string]any{"type": "plain_text_input", "value": "yes"}}}}}})
	if response := serveSignedInteraction(h, body); response.Code != http.StatusOK {
		t.Fatalf("unlinked receipt: %d", response.Code)
	}
	var id uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	var denied bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='identity_unlinked' FROM slack_requests WHERE id=$1`, id).Scan(&denied); err != nil || !denied {
		t.Fatalf("denial not retained: %v %v", denied, err)
	}
	f.link(t)
	if response := serveSignedInteraction(h, body); response.Code != http.StatusOK {
		t.Fatalf("retry: %d", response.Code)
	}
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT a.status='pending' AND r.status='rejected' AND r.error='identity_unlinked' FROM turn_asks a CROSS JOIN slack_requests r WHERE a.id=$1 AND r.id=$2`, ask, id).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("restoration replayed rejected form: %v %v", unchanged, err)
	}
}

func TestSlackInvalidFormRetainsFirstSubmissionAndLeavesAskPending(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	turn, ask := f.pendingQuestion(t)
	c := f.control("answer")
	c.Target.Turn, c.Target.Ask = turn, ask
	h := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	signed, err := encodeControl(h.ControlKey, c)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"type": "view_submission", "view": map[string]any{"callback_id": "helmr.answer", "private_metadata": signed, "state": map[string]any{"values": map[string]any{"text": map[string]any{"text": map[string]any{"type": "plain_text_input"}}}}}}
	if response := serveSignedInteraction(h, formCallbackBody(t, payload)); response.Code != http.StatusOK {
		t.Fatalf("first receipt: %d", response.Code)
	}
	var id uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	var denied bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='rejected' AND r.error='answer_invalid' AND a.status='pending' FROM slack_requests r CROSS JOIN turn_asks a WHERE r.id=$1 AND a.id=$2`, id, ask).Scan(&denied); err != nil || !denied {
		t.Fatalf("invalid form applied: %v %v", denied, err)
	}
	// Retirement preserves the submitted digest and its terminal disposition.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET payload=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, id)
	if response := serveSignedInteraction(h, formCallbackBody(t, payload)); response.Code != http.StatusOK {
		t.Fatalf("retired retry: %d", response.Code)
	}
	payload["view"].(map[string]any)["state"] = map[string]any{"values": map[string]any{"text": map[string]any{"text": map[string]any{"type": "plain_text_input", "value": "fixed"}}}}
	if response := serveSignedInteraction(h, formCallbackBody(t, payload)); response.Code != http.StatusConflict {
		t.Fatalf("changed first submission: %d", response.Code)
	}
}

func TestUnlinkedAnswerOpeningOffersFirstUseWithoutReplayingTheClick(t *testing.T) {
	f := newStatusFixture(t)
	c := f.control("open_answer")
	c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
	h := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	opened := 0
	h.Client = callerFunc(func(_ context.Context, _ uuid.UUID, _ int64, method string, _ []byte) DeliveryResult {
		if method != "views.open" {
			t.Fatal(method)
		}
		opened++
		return DeliveryResult{Disposition: Acknowledged, ViewID: "V1"}
	})
	signed, err := encodeControl(h.ControlKey, c)
	if err != nil {
		t.Fatal(err)
	}
	click := func(at string) []byte {
		return formCallbackBody(t, map[string]any{"type": "block_actions", "trigger_id": "trigger", "actions": []any{map[string]string{"type": "button", "action_id": "helmr.open_answer", "value": signed, "action_ts": at}}})
	}
	body := click(slackNow())
	for range 2 {
		if response := serveSignedInteraction(h, body); response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if opened != 0 {
		t.Fatal("unlinked actor opened answer form")
	}
	feedback, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
	if err != nil || !found || feedback == nil || !strings.Contains(string(feedback.payload), "/auth/slack/connect?link=") {
		t.Fatal(found, feedback, err)
	}
	var private struct {
		Channel string `json:"channel"`
		User    string `json:"user"`
	}
	if err = json.Unmarshal(feedback.payload, &private); err != nil || private.Channel != "C1" || private.User != "human" {
		t.Fatal(private, err)
	}
	f.link(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_user_links SET linked_at=clock_timestamp()`)
	if response := serveSignedInteraction(h, body); response.Code != http.StatusOK || opened != 0 {
		t.Fatal("old click replayed after linking", response.Code, opened)
	}
	if _, found, err = takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig()); err != nil || found {
		t.Fatal("duplicate link hint", found, err)
	}
	var preserved bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM slack_requests)=1 AND EXISTS(SELECT 1 FROM slack_requests WHERE status='rejected' AND error='identity_unlinked' AND payload IS NULL) AND EXISTS(SELECT 1 FROM turn_asks WHERE id=$1 AND status='pending')`, c.Target.Ask).Scan(&preserved); err != nil || !preserved {
		t.Fatal(preserved, err)
	}
	if response := serveSignedInteraction(h, click(slackNow())); response.Code != http.StatusOK || opened != 1 {
		t.Fatal("fresh click could not open current question", response.Code, opened)
	}
}
