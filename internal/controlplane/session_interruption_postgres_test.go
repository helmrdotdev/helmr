package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func interruptedCompletionRequest(t *testing.T, f *actorExecutionFixture, hold uuid.UUID, turn *uuid.UUID) workerapi.CompleteActorRequest {
	t.Helper()
	operation := uuid.NewV7().String()
	f.workerCall(t, f.server.workerBeginRunFinalization, workerapi.BeginRunFinalizationRequest{Lease: f.fence(), OperationID: operation, ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: f.runID.String(), AttemptNumber: f.claim.Attempt().Number, RunLeaseID: f.fence().ID}}, nil)
	interrupted := &workerapi.ActorInterrupted{HoldID: hold.String()}
	if turn != nil {
		id := turn.String()
		interrupted.TurnID = &id
	}
	return workerapi.CompleteActorRequest{Lease: f.fence(), OperationID: operation, Outcome: workerapi.ActorOutcome{RunGeneration: f.claim.Session().RunGeneration, Interrupted: interrupted}}
}

func TestSessionInterruptedCompletionRejectsChangedHoldPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	req := interruptedCompletionRequest(t, f, stopped.HoldID, &scope.TurnID)
	for _, change := range []string{"hold", "generation", "turn", "success"} {
		t.Run(change, func(t *testing.T) {
			copy := req
			outcome := *req.Outcome.Interrupted
			copy.Outcome.Interrupted = &outcome
			switch change {
			case "hold":
				copy.Outcome.Interrupted.HoldID = uuid.NewV7().String()
			case "generation":
				copy.Outcome.RunGeneration++
			case "turn":
				id := uuid.NewV7().String()
				copy.Outcome.Interrupted.TurnID = &id
			case "success":
				copy.Outcome.Interrupted = nil
				copy.Outcome.Succeeded = &workerapi.ActorSucceeded{}
			}
			parsed, err := parseActorCompletionRequest(copy)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.server.completeActor(t.Context(), f.worker, copy, parsed); !errors.Is(err, errStaleActorCompletion) {
				t.Fatalf("unexpected completion: %v", err)
			}
		})
	}
	var actor db.Session
	actor, err = f.server.db.GetActor(t.Context(), db.GetActorParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(f.sessionID)})
	if err != nil || actor.CommittedInputSequence != 0 || actor.DispatchHoldReason.String != "interrupt_requested" {
		t.Fatalf("rejected proof changed state: %+v %v", actor, err)
	}
}

func TestSessionBetweenTurnsInterruptionPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	canceler, err := run.NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = canceler.Cancel(t.Context(), run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: f.runID}); err != nil {
		t.Fatal(err)
	}
	var hold uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	req := interruptedCompletionRequest(t, f, hold, nil)
	parsed, err := parseActorCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	var active, current *uuid.UUID
	var reason string
	var terminalCount int
	if err = f.Pool.QueryRow(t.Context(), `SELECT committed_input_sequence,active_turn_id,current_run_id,dispatch_hold_reason,(SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='turn.interrupted') FROM sessions WHERE id=$1`, f.sessionID).Scan(&cursor, &active, &current, &reason, &terminalCount); err != nil {
		t.Fatal(err)
	}
	if cursor != 0 || active != nil || current != nil || reason != "interrupted" || terminalCount != 0 {
		t.Fatalf("between-Turn stop fabricated Turn: %d %v %v %s %d", cursor, active, current, reason, terminalCount)
	}
}

func TestSessionInterruptedCompletionRejectsUnacknowledgedMessagePostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	readyMessages(t, f, scope)
	admitMessage(t, f, "pending")
	claimMessage(t, f, scope)
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	req := interruptedCompletionRequest(t, f, stopped.HoldID, &scope.TurnID)
	parsed, err := parseActorCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.completeActor(t.Context(), f.worker, req, parsed); !errors.Is(err, errStaleActorCompletion) {
		t.Fatalf("unacknowledged callback settled: %v", err)
	}
}

func TestSessionHotChildCallStopConvergesPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	manifest, digest, err := definition.CanonicalManifestAndDigest([]byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Pool.Exec(t.Context(), `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, f.TaskDefinitionID, manifest, digest[:]); err != nil {
		t.Fatal(err)
	}
	turnID := scope.TurnID.String()
	cursor := int64(1)
	target, _ := json.Marshal(map[string]string{"id": f.computerID.String()})
	request := workerapi.InvokeChildTaskRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Computer: target, Options: json.RawMessage(`{}`), IdempotencyKey: "stop-child", TurnID: &turnID, RunGeneration: &scope.RunGeneration, ActorSpeculativeInputSequence: &cursor}
	var response workerapi.InvokeChildTaskResponse
	f.workerCall(t, f.server.workerInvokeChildTask, request, &response)
	if response.OpenedWait == nil || response.Failed != nil {
		t.Fatalf("child call: %+v", response)
	}
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	var childState, rootState string
	if err = f.Pool.QueryRow(t.Context(), `SELECT coalesce(c.status,'not_started'),r.status FROM run_waits w LEFT JOIN runs c ON c.id=w.child_run_id JOIN runs r ON r.id=w.run_id WHERE w.id=$1`, request.RunWaitID).Scan(&childState, &rootState); err != nil {
		t.Fatal(err)
	}
	if childState != "cancelled" || rootState != "running" {
		t.Fatalf("owned stop: root=%s child=%s", rootState, childState)
	}
	req := interruptedCompletionRequest(t, f, stopped.HoldID, &scope.TurnID)
	parsed, err := parseActorCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
		t.Fatal(err)
	}
}

func TestSessionControlObservationDoesNotLockWorkerSupplyPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM worker_groups WHERE id=$1 FOR UPDATE`, f.worker.GroupID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	state, err := f.server.db.ReadWorkerSessionControl(ctx, db.ReadWorkerSessionControlParams{RunLeaseID: f.claim.Lease().ID, LeaseSequence: f.fence().LeaseSequence, WorkerGroupID: pgvalue.UUID(f.worker.GroupID), WorkerHostID: pgvalue.UUID(f.worker.HostID), WorkerEpoch: f.worker.Epoch, RunGeneration: scope.RunGeneration})
	if err != nil {
		t.Fatalf("advisory read blocked on supply mutation: %v", err)
	}
	if state.DispatchHoldID != pgvalue.UUID(stopped.HoldID) || state.ActiveTurnID != pgvalue.UUID(scope.TurnID) {
		t.Fatalf("wrong control: %+v", state)
	}
}
