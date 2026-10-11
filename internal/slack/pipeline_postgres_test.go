package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestSlackWorkerPipelineAdmitsProjectsAnswersAndJoins(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	receipt := f.reply(t, "pipeline", false)
	config := testProjectionConfig()
	var mu sync.Mutex
	calls := 0
	metadata := ""
	client := deliveryTestClient(func(r *http.Request) string {
		var body struct {
			Status string `json:"status"`
			View   struct {
				Metadata string `json:"private_metadata"`
			} `json:"view"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch r.URL.Path {
		case "/api/agents.sessions.setStatus":
			return fmt.Sprintf(`{"ok":true,"agent_status":%q}`, body.Status)
		case "/api/views.open":
			metadata = body.View.Metadata
			return `{"ok":true,"view":{"id":"V1"}}`
		default:
			return fmt.Sprintf(`{"ok":true,"ts":"123.%d","channel":"C1"}`, 900+calls)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	done := make(chan error, 3)
	go func() { done <- RunRequests(ctx, f.Pool, nil, config, client, log) }()
	go func() { done <- RunProjection(ctx, f.Pool, config, log) }()
	go func() { done <- RunDelivery(ctx, f.Pool, client, log) }()
	defer func() {
		if t.Failed() {
			var state string
			err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('posts',(SELECT jsonb_agg(jsonb_build_object('role',role,'status',status,'error',error,'attempts',attempt_count,'next',next_attempt_at,'stream',stream_state)) FROM slack_posts),'thread',(SELECT jsonb_agg(jsonb_build_object('status',desired_status,'confirmation',status_confirmation,'error',delivery_error,'repair',stream_status_repair)) FROM slack_threads))::text`).Scan(&state)
			t.Log("pipeline state", state, err)
		}
		cancel()
		for range 3 {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Errorf("worker did not join cancellation: %v", err)
			}
		}
	}()
	wait := func(label, query string, args ...any) {
		t.Helper()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			var ok bool
			if err := f.Pool.QueryRow(ctx, query, args...).Scan(&ok); err != nil {
				t.Fatal(label, err)
			}
			if ok {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(label, ctx.Err())
			case <-ticker.C:
			}
		}
	}
	wait("admission and native processing status", `SELECT EXISTS(SELECT 1 FROM slack_requests r JOIN slack_threads t ON t.id=r.thread_id WHERE r.id=$1 AND r.status='accepted' AND t.desired_status='processing' AND t.status_confirmation='acknowledged' AND t.confirmed_revision=t.desired_revision) AND NOT EXISTS(SELECT 1 FROM slack_posts WHERE request_id=$1)`, receipt)
	var turn uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT turn_id FROM slack_requests WHERE id=$1`, receipt).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	if _, err := agent.Dispatch(ctx, f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.RuntimeOutput(ctx, f.Pool, host, execution, turn, uuid.NewV7(), json.RawMessage(`"runner progress"`)); err != nil {
		t.Fatal(err)
	}
	ask := uuid.NewV7()
	if err := agent.RuntimeAsk(ctx, f.Pool, host, execution, turn, ask, json.RawMessage(`{"prompt":[{"type":"text","text":"Continue?"}],"answer":{"type":"text"}}`)); err != nil {
		t.Fatal(err)
	}
	wait("question delivered", `SELECT EXISTS(SELECT 1 FROM slack_posts WHERE ask_id=$1 AND status='posted')`, ask)
	var raw []byte
	if err := f.Pool.QueryRow(ctx, `SELECT payload FROM slack_posts WHERE ask_id=$1`, ask).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Blocks []struct {
			Elements []struct {
				ID    string `json:"action_id"`
				Value string `json:"value"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	signed := ""
	for _, block := range body.Blocks {
		for _, button := range block.Elements {
			if button.ID == "helmr.open_answer" {
				signed = button.Value
			}
		}
	}
	if signed == "" {
		t.Fatal("no exact answer control")
	}
	h := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: config.ControlKey, Client: client}
	open := formCallbackBody(t, map[string]any{"type": "block_actions", "trigger_id": "trigger", "actions": []any{map[string]string{"type": "button", "action_id": "helmr.open_answer", "value": signed, "action_ts": slackNow()}}})
	if response := serveSignedInteraction(h, open); response.Code != http.StatusOK {
		t.Fatalf("open=%d", response.Code)
	}
	mu.Lock()
	signedForm := metadata
	mu.Unlock()
	if signedForm == "" {
		t.Fatal("modal not opened")
	}
	submit := formCallbackBody(t, map[string]any{"type": "view_submission", "view": map[string]any{"callback_id": "helmr.answer", "private_metadata": signedForm, "state": map[string]any{"values": map[string]any{"text": map[string]any{"text": map[string]any{"type": "plain_text_input", "value": "PRIVATE_ANSWER"}}}}}})
	for range 2 {
		if response := serveSignedInteraction(h, submit); response.Code != http.StatusOK {
			t.Fatalf("submit=%d", response.Code)
		}
	}
	wait("answer consumed and card updated", `SELECT EXISTS(SELECT 1 FROM turn_asks a JOIN slack_posts p ON p.ask_id=a.id WHERE a.id=$1 AND a.status='responded' AND p.status='posted' AND p.confirmed_revision=p.desired_revision AND p.desired_revision=2)`, ask)
	wait("progress delivered", `SELECT EXISTS(SELECT 1 FROM slack_posts WHERE turn_id=$1 AND role='intermediate' AND status='posted')`, turn)
	var correct bool
	if err := f.Pool.QueryRow(ctx, `SELECT (SELECT count(*)=1 FROM turns WHERE session_id=$1) AND (SELECT count(*)=1 FROM slack_requests WHERE ask_id=$2 AND status='accepted') AND NOT EXISTS(SELECT 1 FROM slack_posts WHERE convert_from(payload,'UTF8') LIKE '%PRIVATE_ANSWER%')`, f.Session, ask).Scan(&correct); err != nil || !correct {
		t.Fatalf("answer leaked or work duplicated: %v %v", correct, err)
	}
}
