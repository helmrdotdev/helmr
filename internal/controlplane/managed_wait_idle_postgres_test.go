package controlplane

import (
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestManagedWaitIdlePolicyPostgres(t *testing.T) {
	for _, kind := range []workerapi.RunWaitKind{"token", "timer", "actor_input"} {
		for _, override := range []bool{false, true} {
			name := string(kind) + "/inherited"
			if override {
				name = string(kind) + "/explicit"
			}
			t.Run(name, func(t *testing.T) {
				f := newActorCheckpointFixture(t)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=jsonb_set(manifest, '{idleTimeoutMs}', '300000') WHERE environment_id=$1 AND kind='actor'`, f.EnvironmentID)
				id := uuid.NewV7()
				cursor := int64(0)
				request := workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: id.String(), ResumeAttachID: uuid.NewV7().String(), Kind: kind, ActorSpeculativeInputSequence: &cursor}
				if kind == "actor_input" {
					request.Params, _ = json.Marshal(map[string]any{"session_id": f.sessionID.String(), "after_input_sequence": 0})
				} else {
					scope := f.receiveTurn(t, 1)
					cursor = 1
					request.TurnID = new(scope.TurnID.String())
					request.RunGeneration = new(scope.RunGeneration)
					if kind == "token" {
						_, registration := actorTokenWait(t, f, scope)
						request.Params, _ = json.Marshal(map[string]string{"token_id": registration.TokenID.String()})
					} else {
						request.Params = json.RawMessage(`{"duration":"1h"}`)
						request.TimeoutMS = new(int64(time.Hour / time.Millisecond))
					}
				}
				want := 5 * time.Minute
				if override {
					want = 10 * time.Minute
					request.IdleTimeoutMS = new(want.Milliseconds())
				}
				before := time.Now()
				f.workerCall(t, f.server.workerCreateRunWait, request, nil)
				after := time.Now()
				var idle int64
				var due time.Time
				if err := f.Pool.QueryRow(t.Context(), `SELECT idle_timeout_ms, checkpoint_due_at FROM run_waits WHERE id=$1`, id).Scan(&idle, &due); err != nil {
					t.Fatal(err)
				}
				if idle != want.Milliseconds() || due.Before(before.Add(want)) || due.After(after.Add(want)) {
					t.Fatalf("idle=%d due=%s want registration + %s", idle, due, want)
				}
				if kind == "timer" {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET checkpoint_due_at=now()-interval '1 second' WHERE id=$1`, id)
					if override {
						f.publishWaitCheckpoint(t, id, f.capture(t, "timer state"))
					} else {
						var poll workerapi.RunWaitPollResponse
						f.workerCall(t, f.server.workerPollRunWait, workerapi.RunWaitPollRequest{Lease: f.fence(), RunWaitID: id.String()}, &poll)
						if poll.Status != workerapi.RunWaitPollStatusCheckpointRequested {
							t.Fatalf("timer did not request checkpoint: %+v", poll)
						}
					}
				}
			})
		}
	}
}
