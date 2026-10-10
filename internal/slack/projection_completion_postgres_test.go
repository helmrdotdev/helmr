package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestSlackCommittedResponseWaitsForCoreCompletionAndIntermediateDrain(t *testing.T) {
	for _, kind := range []string{"text", "empty", "quiet", "cancelled", "failed", "stream", "stream-status-warning"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			if kind == "stream" || kind == "stream-status-warning" {
				f.link(t)
			}
			write := f.outputWriter(t)
			write("visible progress")
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			response := json.RawMessage(`"committed answer"`)
			if kind == "empty" {
				response = json.RawMessage(`[]`)
			}
			if kind != "quiet" {
				if err := agent.RuntimeRespond(t.Context(), f.Pool, host, execution, turn, uuid.NewV7(), response); err != nil {
					t.Fatal(err)
				}
			}
			assertResponses := func(want int) {
				t.Helper()
				var n int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE role='response'`).Scan(&n); err != nil || n != want {
					t.Fatalf("response count=%d want=%d: %v", n, want, err)
				}
			}
			f.projectAll(t)
			assertResponses(0)
			if err := agent.RuntimeCloseProcessing(t.Context(), f.Pool, host, execution, turn); err != nil {
				t.Fatal(err)
			}
			if kind != "failed" {
				if outcome, err := agent.RuntimeFinalize(t.Context(), f.Pool, host, execution, turn, json.RawMessage(`{"private":"PRIVATE_RESULT"}`), "callbacks drained"); err != nil || outcome != nil {
					t.Fatalf("finalize: %s %v", outcome, err)
				}
				if err := agent.Complete(t.Context(), f.Pool, f.Environment, f.Session, turn); !errors.Is(err, agent.ErrNotReady) {
					t.Fatalf("unpublished completion=%v", err)
				}
				f.projectAll(t)
				assertResponses(0)
			}
			if kind == "failed" {
				if _, err := agent.RuntimeFail(t.Context(), f.Pool, host, execution, turn, agent.TurnError{Code: "application_failure", Message: "PRIVATE_FAILURE"}, "callbacks drained"); err != nil {
					t.Fatal(err)
				}
			} else if kind == "cancelled" {
				if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
					t.Fatal(err)
				}
			} else {
				// Certified storage is fixture state here; the real Complete API remains
				// responsible for the own-save gate, event and publication transaction.
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_saves s SET status='published',flush_acknowledged_at=statement_timestamp(),captured_at=statement_timestamp(),captured_root_digest=c.initial_root_digest,root_id=c.initial_root_id,capture_evidence='fixture cut',publication_evidence='fixture publication' FROM computers c WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.turn_id=$1`, turn)
				for range 2 {
					if err := agent.Complete(t.Context(), f.Pool, f.Environment, f.Session, turn); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.projectAll(t)
			f.projectAll(t)
			want := 1
			if kind == "quiet" || kind == "cancelled" || kind == "failed" {
				want = 0
			}
			assertResponses(want)
			var final uuid.UUID
			if want == 1 {
				if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_posts WHERE role='response'`).Scan(&final); err != nil {
					t.Fatal(err)
				}
				f.noPostClaim(t, final)
			}
			var calls []string
			client := deliveryTestClient(func(r *http.Request) string {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(raw, []byte("PRIVATE_RESULT")) || bytes.Contains(raw, []byte("PRIVATE_FAILURE")) {
					t.Fatal("private result published")
				}
				calls = append(calls, r.URL.Path+" "+string(raw))
				var payload struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(raw, &payload)
				if r.URL.Path == "/api/agents.sessions.setStatus" {
					if kind == "stream-status-warning" {
						return fmt.Sprintf(`{"ok":true,"agent_status":%q,"response_metadata":{"warnings":["missing_agent_session_stopped_event_subscription"]}}`, payload.Status)
					}
					return fmt.Sprintf(`{"ok":true,"agent_status":%q}`, payload.Status)
				}
				return fmt.Sprintf(`{"ok":true,"ts":"123.%d","channel":"C1"}`, 800+len(calls))
			})
			settled := false
			for range 20 {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET delivery_next_at=clock_timestamp(); UPDATE slack_posts SET next_attempt_at=clock_timestamp(); UPDATE slack_threads SET next_attempt_at=clock_timestamp()`, pgx.QueryExecModeSimpleProtocol)
				if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 64); err != nil {
					t.Fatal(err)
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM slack_posts WHERE status<>'posted' OR confirmed_revision<>desired_revision OR (presentation_path='stream' AND stream_state<>'stopped'))`).Scan(&settled); err != nil {
					t.Fatal(err)
				}
				if settled {
					break
				}
			}
			if !settled {
				t.Fatalf("delivery did not settle: %v", calls)
			}
			joined := []byte(fmt.Sprint(calls))
			if (kind == "quiet" || kind == "cancelled" || kind == "failed") && bytes.Contains(joined, []byte("committed answer")) {
				t.Fatal("uncommitted response sent")
			}
			if (kind == "text" || kind == "stream" || kind == "stream-status-warning") && !bytes.Contains(joined, []byte("committed answer")) {
				t.Fatal("committed response missing")
			}
			if kind == "empty" && !bytes.Contains(joined, []byte("Completed.")) {
				t.Fatal("explicit empty response has no completion label")
			}
			if (kind == "stream" || kind == "stream-status-warning") && !bytes.Contains(joined, []byte("/api/chat.stopStream")) {
				t.Fatal("stream never stopped")
			}
			assertResponses(want)
		})
	}
}
