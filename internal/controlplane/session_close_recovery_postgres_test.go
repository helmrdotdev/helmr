package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestSessionCloseSettlesStoppedExecutionThroughDeliveryPostgres(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "drained recovery closes without resume"
		if queued {
			name = "queued work resumes after delivered close"
		}
		t.Run(name, func(t *testing.T) {
			f := newSessionHTTP(t, sessiontest.New(t, 1))
			started := startSession(t, f.Fixture, 0, nil, "")
			token := f.apiKey(auth.Principal{OrgID: f.OrgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleOwner, ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String(), Permissions: []auth.Permission{auth.PermissionRunsManage, auth.PermissionSessionsSend, auth.PermissionSessionsClose, auth.PermissionSessionsResume}})
			call := func(route string, raw any, result any) {
				t.Helper()
				body, err := json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
				w := f.request(t, http.MethodPost, "/v1/sessions/"+started.SessionID.String()+route, token, string(body))
				if w.Code != http.StatusAccepted {
					t.Fatalf("operation=%d %s", w.Code, w.Body.String())
				}
				if result != nil {
					if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
						t.Fatal(err)
					}
				}
			}
			if queued {
				call("/enqueue", api.SessionDataRequest{Data: json.RawMessage(`"next"`), IdempotencyKey: "next"}, nil)
			}
			w := f.request(t, http.MethodPost, "/v1/runs/"+started.BootRunID.String()+"/cancel", token, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("cancel=%d %s", w.Code, w.Body.String())
			}
			var stopped api.ActorRunCancellationReceipt
			if err := json.Unmarshal(w.Body.Bytes(), &stopped); err != nil {
				t.Fatal(err)
			}
			var closeReceipt api.SessionCloseReceipt
			call("/close", api.CloseSessionRequest{IdempotencyKey: "close"}, &closeReceipt)

			reconciler, err := session.NewReconciler(f.Pool)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := session.NewDeliveryWorker(nil, db.New(f.Pool), reconciler.ReconcileInput, reconciler.ReconcileLifecycle)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- worker.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Errorf("delivery worker: %v", err)
				}
			})
			waitDelivered := func(id string) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for {
					var status string
					if err := f.Pool.QueryRow(ctx, `SELECT status FROM control_outbox WHERE id=$1 AND topic='session.lifecycle.reconcile'`, id).Scan(&status); err != nil {
						t.Fatal(err)
					}
					if status == "delivered" {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("close delivery %s remained %s", id, status)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			waitDelivered(closeReceipt.ID)
			var status string
			var hold, current, sessionComputer *uuid.UUID
			read := func() {
				t.Helper()
				if err := f.Pool.QueryRow(ctx, `SELECT s.status,s.dispatch_hold_id,s.current_run_id,s.computer_id FROM sessions s JOIN computers w ON w.id=s.computer_id WHERE s.id=$1`, started.SessionID).Scan(&status, &hold, &current, &sessionComputer); err != nil {
					t.Fatal(err)
				}
			}
			read()
			if queued {
				if status != "closing" || hold == nil || current != nil || sessionComputer == nil {
					t.Fatalf("stopped execution dispatched held input: %s hold=%v run=%v sessionComputer=%v", status, hold, current, sessionComputer)
				}
				var resumed api.SessionResumeReceipt
				call("/resume", api.ResumeSessionRequest{HoldID: hold.String(), IdempotencyKey: "resume"}, &resumed)
				waitDelivered(resumed.ID)
				read()
				if status != "closing" || hold != nil || current == nil || *current == started.BootRunID || sessionComputer == nil {
					t.Fatalf("resume lost continuation: %s hold=%v run=%v sessionComputer=%v", status, hold, current, sessionComputer)
				}
			} else if status != "closed" || hold != nil || current != nil || sessionComputer == nil {
				t.Fatalf("drained stop did not close: %s hold=%v run=%v sessionComputer=%v", status, hold, current, sessionComputer)
			}
		})
	}
}
