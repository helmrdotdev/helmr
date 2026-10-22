package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestSlackCommittedResponseWaitsForCoreCompletion(t *testing.T) {
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
			execution := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			response := json.RawMessage(`"committed answer"`)
			if kind == "empty" {
				response = json.RawMessage(`[]`)
			}
			if kind != "quiet" {
				if err := session.RuntimeRespond(t.Context(), f.Pool, host, execution, turn, uuid.NewV7(), response); err != nil {
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
			if err := session.RuntimeCloseProcessing(t.Context(), f.Pool, host, execution, turn); err != nil {
				t.Fatal(err)
			}
			if kind != "failed" {
				if outcome, err := session.RuntimeFinalize(t.Context(), f.Pool, host, execution, turn, json.RawMessage(`{"private":"PRIVATE_RESULT"}`), "callbacks drained"); err != nil || outcome == nil {
					t.Fatalf("finalize: %s %v", outcome, err)
				}
				// Completion and delivery intent are committed while the save
				// remains requested. No storage fixture publication is needed.
				var requested bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT status='requested' FROM computer_saves WHERE turn_id=$1`, turn).Scan(&requested); err != nil || !requested {
					t.Fatal("save did not remain pending", requested, err)
				}
			}
			if kind == "failed" {
				if _, err := session.RuntimeFail(t.Context(), f.Pool, host, execution, turn, session.TurnError{Code: "application_failure", Message: "PRIVATE_FAILURE"}, "callbacks drained"); err != nil {
					t.Fatal(err)
				}
			} else if kind == "cancelled" {
				if _, err := session.ControlSession(t.Context(), f.Pool, session.Caller{Kind: "user", ID: f.User}, session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
					t.Fatal(err)
				}
			}
			f.projectAll(t)
			f.projectAll(t)
			want := 1
			if kind == "quiet" || kind == "failed" {
				want = 0
			}
			assertResponses(want)
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
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_delivery_limits SET next_post_at=clock_timestamp(),next_status_at=clock_timestamp(); UPDATE slack_method_limits SET retry_after=clock_timestamp(); UPDATE slack_posts SET next_attempt_at=clock_timestamp(); UPDATE slack_threads SET next_attempt_at=clock_timestamp()`, pgx.QueryExecModeSimpleProtocol)
				if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 64); err != nil {
					t.Fatal(err)
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM slack_posts WHERE status<>'posted' OR confirmed_revision<>desired_revision OR (presentation_path='stream' AND stream_status<>'stopped'))`).Scan(&settled); err != nil {
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
			if (kind == "quiet" || kind == "failed") && bytes.Contains(joined, []byte("committed answer")) {
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
