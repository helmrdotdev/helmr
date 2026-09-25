package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestSessionCloseSettlesStoppedExecutionThroughDeliveryPostgres(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "drained recovery closes without resume"
		if queued {
			name = "queued work resumes after delivered close"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorStartPostgresFixture(t, 1)
			started, err := f.server.startActor(t.Context(), f.request(0, nil, ""))
			if err != nil {
				t.Fatal(err)
			}
			principal := auth.Actor{OrgID: f.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleOwner, ProjectID: f.projectID.String(), EnvironmentID: f.environmentID.String(), Permissions: []auth.Permission{auth.PermissionRunsManage, auth.PermissionSessionsSend, auth.PermissionSessionsClose, auth.PermissionSessionsResume}}
			call := func(handler http.HandlerFunc, raw any, result any) {
				t.Helper()
				body, err := json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				handler(w, sessionLifecycleRequest(string(body), principal, started.SessionID.String(), ""))
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
				call(f.server.enqueueSessionHTTP, api.SessionDataRequest{Data: json.RawMessage(`"next"`), IdempotencyKey: "next"}, nil)
			}
			w := httptest.NewRecorder()
			f.server.cancelRunHTTP(w, runCancellationRequest(t, started.BootRunID.String(), principal))
			if w.Code != http.StatusAccepted {
				t.Fatalf("cancel=%d %s", w.Code, w.Body.String())
			}
			var stopped api.ActorRunCancellationReceipt
			if err := json.Unmarshal(w.Body.Bytes(), &stopped); err != nil {
				t.Fatal(err)
			}
			var closeReceipt api.SessionCloseReceipt
			call(f.server.closeSessionHTTP, api.CloseSessionRequest{IdempotencyKey: "close"}, &closeReceipt)

			reconciler, err := session.NewReconciler(f.pool)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := session.NewDeliveryWorker(nil, db.New(f.pool), reconciler.ReconcileInput, reconciler.ReconcileLifecycle)
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
					if err := f.pool.QueryRow(ctx, `SELECT status FROM control_outbox WHERE id=$1 AND topic='session.lifecycle.reconcile'`, id).Scan(&status); err != nil {
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
			var hold, current, owner *uuid.UUID
			read := func() {
				t.Helper()
				if err := f.pool.QueryRow(ctx, `SELECT s.status,s.dispatch_hold_id,s.current_run_id,w.owner_session_id FROM sessions s JOIN computers w ON w.id=s.workspace_id WHERE s.id=$1`, started.SessionID).Scan(&status, &hold, &current, &owner); err != nil {
					t.Fatal(err)
				}
			}
			read()
			if queued {
				if status != "closing" || hold == nil || current != nil || owner == nil {
					t.Fatalf("stopped execution dispatched held input: %s hold=%v run=%v owner=%v", status, hold, current, owner)
				}
				var resumed api.SessionResumeReceipt
				call(f.server.resumeSessionHTTP, api.ResumeSessionRequest{HoldID: hold.String(), IdempotencyKey: "resume"}, &resumed)
				waitDelivered(resumed.ID)
				read()
				if status != "closing" || hold != nil || current == nil || *current == started.BootRunID || owner == nil {
					t.Fatalf("resume lost continuation: %s hold=%v run=%v owner=%v", status, hold, current, owner)
				}
			} else if status != "closed" || hold != nil || current != nil || owner != nil {
				t.Fatalf("drained stop did not close: %s hold=%v run=%v owner=%v", status, hold, current, owner)
			}
		})
	}
}
