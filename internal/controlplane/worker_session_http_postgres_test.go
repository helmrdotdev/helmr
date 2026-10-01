package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// workerOutcome is the part of a worker Session route's 200 response that
// reports its outcome.
type workerOutcome struct {
	CorrelationID string                             `json:"correlation_id"`
	Accepted      bool                               `json:"accepted"`
	Completed     json.RawMessage                    `json:"completed"`
	Failed        *workerapi.RuntimeOperationFailure `json:"failed"`
}

// A worker Session route whose lease receipt no longer addresses the live
// execution answers with its stale outcome: a 200 stale_execution failure for
// the operations that authorize a source Run, and a conflict for the
// operations on the worker's own Actor execution.
func TestWorkerSessionRoutesReportStaleLeasePostgres(t *testing.T) {
	f := newActorExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	stale := f.fence()
	stale.LeaseSequence++
	session := f.SessionID.String()
	turn := scope.TurnID.String()
	correlation := uuid.NewV7().String()
	reference := workerapi.SessionReferenceRequest{Lease: stale, CorrelationID: correlation, SessionID: session}
	command := workerapi.TurnExecutionRequest{Lease: stale, CorrelationID: correlation, TurnID: turn, RunGeneration: scope.RunGeneration}
	after := int64(0)
	waitParams, _ := json.Marshal(workerSessionInputWaitParams{SessionID: session, AfterInputSequence: after})
	for _, test := range []struct {
		path    string
		body    any
		status  int
		message string
	}{
		{"/run/sessions/send", workerapi.SubmitSessionDataRequest{Lease: stale, CorrelationID: correlation, SessionID: session, Data: json.RawMessage(`1`)}, http.StatusOK, ""},
		{"/run/sessions/enqueue", workerapi.SubmitSessionDataRequest{Lease: stale, CorrelationID: correlation, SessionID: session, Data: json.RawMessage(`1`)}, http.StatusOK, ""},
		{"/run/turns/messages/send", workerapi.SubmitSessionDataRequest{Lease: stale, CorrelationID: correlation, SessionID: session, Data: json.RawMessage(`1`), TurnID: &turn}, http.StatusOK, ""},
		{"/run/sessions/close", workerapi.CloseSessionRequest{SessionReferenceRequest: reference}, http.StatusOK, ""},
		{"/run/sessions/events/read-page", workerapi.ReadSessionEventsRequest{SessionReferenceRequest: reference, Limit: 10}, http.StatusOK, ""},
		{"/run/sessions/cancel", workerapi.CancelSessionRequest{SessionReferenceRequest: reference}, http.StatusOK, ""},
		{"/run/turns/interrupt", workerapi.InterruptSessionTurnRequest{TurnReferenceRequest: workerapi.TurnReferenceRequest{SessionReferenceRequest: reference, TurnID: turn}}, http.StatusOK, ""},
		{"/run/sessions/resume", workerapi.ResumeSessionRequest{SessionReferenceRequest: reference, HoldID: uuid.NewV7().String()}, http.StatusOK, ""},
		{"/run/turns/retrieve", workerapi.TurnReferenceRequest{SessionReferenceRequest: reference, TurnID: turn}, http.StatusOK, ""},
		{"/run/sessions/control", workerapi.SessionControlRequest{Lease: stale, CorrelationID: correlation, RunGeneration: scope.RunGeneration}, http.StatusOK, ""},
		{"/run/sessions/retrieve", reference, http.StatusConflict, "run source authority is stale"},
		{"/run/turns/messages/ready", command, http.StatusConflict, "actor output append source authority is stale\nno rows in result set"},
		{"/run/turns/messages/claim", workerapi.ClaimTurnMessageRequest{TurnExecutionRequest: command, DeliveryID: uuid.NewV7().String()}, http.StatusConflict, "actor output append source authority is stale\nno rows in result set"},
		{"/run/turns/messages/complete", workerapi.CompleteTurnMessageRequest{TurnExecutionRequest: command, MessageID: uuid.NewV7().String(), DeliveryID: uuid.NewV7().String(), Status: "handled"}, http.StatusConflict, "actor output append source authority is stale\nno rows in result set"},
		{"/run/turns/settlement/begin", command, http.StatusConflict, "actor output append source authority is stale\nno rows in result set"},
		{"/run/sessions/output/write", workerapi.WriteSessionOutputRequest{Lease: stale, CorrelationID: correlation, RunGeneration: scope.RunGeneration, Data: json.RawMessage(`1`)}, http.StatusConflict, "actor output append source authority is stale\nno rows in result set"},
		{"/run/turns/output/write", workerapi.WriteTurnOutputRequest{Lease: stale, CorrelationID: correlation, TurnID: turn, RunGeneration: scope.RunGeneration, Data: json.RawMessage(`1`)}, http.StatusConflict, "actor output append source authority is stale"},
		{"/run/sessions/turns/commit", workerapi.CommitActorTurnRequest{Lease: stale, CorrelationID: correlation, TurnID: turn, RunGeneration: scope.RunGeneration, Disposition: "completed", Result: json.RawMessage(`1`), TargetInputSequence: 1}, http.StatusConflict, "actor turn commit is stale"},
		{"/run/sessions/complete", workerapi.CompleteActorRequest{Lease: stale, OperationID: uuid.NewV7().String(), Outcome: workerapi.ActorOutcome{RunGeneration: scope.RunGeneration, Succeeded: &workerapi.ActorSucceeded{}}}, http.StatusConflict, "actor completion receipt is stale"},
		{"/run/waits/create", workerapi.CreateRunWaitRequest{CorrelationID: correlation, Lease: stale, RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: waitParams, ActorSpeculativeInputSequence: &after}, http.StatusConflict, "worker run wait receipt is stale"},
	} {
		t.Run(test.path, func(t *testing.T) {
			w := f.worker(t, test.path, test.body)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, test.status, w.Body.String())
			}
			if test.status != http.StatusOK {
				if got := decodeHTTPError(t, w.Body.Bytes()); got.Code != "conflict" || got.Message != test.message {
					t.Fatalf("error = %+v, want conflict %q", got, test.message)
				}
				return
			}
			var outcome workerOutcome
			if err := json.Unmarshal(w.Body.Bytes(), &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.CorrelationID != correlation || outcome.Accepted || outcome.Completed != nil || outcome.Failed == nil || outcome.Failed.Code != "stale_execution" {
				t.Fatalf("outcome = %s", w.Body.String())
			}
		})
	}
}

// A malformed lease receipt is internal for the operations that parse it as
// a lease, a stale source for a Session retrieve and a stale_execution
// failure for a Turn retrieve.
func TestWorkerSessionRoutesReportMalformedLeasePostgres(t *testing.T) {
	f := newActorExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	malformed := workerapi.RunLeaseFence{ID: "not-a-lease", LeaseSequence: 1}
	reference := workerapi.SessionReferenceRequest{Lease: malformed, CorrelationID: uuid.NewV7().String(), SessionID: f.SessionID.String()}
	if w := f.worker(t, "/run/sessions/send", workerapi.SubmitSessionDataRequest{Lease: malformed, CorrelationID: reference.CorrelationID, SessionID: reference.SessionID, Data: json.RawMessage(`1`)}); w.Code != http.StatusInternalServerError {
		t.Fatalf("send = %d %s", w.Code, w.Body.String())
	}
	if w := f.worker(t, "/run/turns/messages/ready", workerapi.TurnExecutionRequest{Lease: malformed, CorrelationID: reference.CorrelationID, TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration}); w.Code != http.StatusInternalServerError {
		t.Fatalf("readiness = %d %s", w.Code, w.Body.String())
	}
	if w := f.worker(t, "/run/sessions/retrieve", reference); w.Code != http.StatusConflict {
		t.Fatalf("Session retrieve = %d %s", w.Code, w.Body.String())
	}
	var outcome workerOutcome
	f.workerCall(t, "/run/turns/retrieve", workerapi.TurnReferenceRequest{SessionReferenceRequest: reference, TurnID: scope.TurnID.String()}, &outcome)
	if outcome.Failed == nil || outcome.Failed.Code != "stale_execution" {
		t.Fatalf("Turn retrieve = %+v", outcome)
	}
}

// The worker Session routes serve the session owner's operations and report
// its committed rejections and Turn failures as 200 failures.
func TestWorkerSessionRoutesServeSessionOperationsPostgres(t *testing.T) {
	f := newActorExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	reference := func(sessionID string) workerapi.SessionReferenceRequest {
		return workerapi.SessionReferenceRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), SessionID: sessionID}
	}
	self := f.SessionID.String()
	missing := uuid.NewV7().String()

	var outcome workerOutcome
	f.workerCall(t, "/run/sessions/send", workerapi.SubmitSessionDataRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), SessionID: missing, Data: json.RawMessage(`1`)}, &outcome)
	if outcome.Failed == nil || outcome.Failed.Code != "session_not_found" {
		t.Fatalf("send to missing Session = %+v", outcome)
	}
	var status workerapi.SessionStatusResponse
	f.workerCall(t, "/run/sessions/retrieve", reference(missing), &status)
	if status.Failed == nil || status.Failed.Code != "session_not_found" || status.Failed.Message != "Session was not found" {
		t.Fatalf("retrieve missing Session = %+v", status)
	}
	status = workerapi.SessionStatusResponse{}
	f.workerCall(t, "/run/sessions/retrieve", reference(self), &status)
	if status.Completed == nil || status.Completed.ID != self || status.Completed.ActiveTurnID == nil || *status.Completed.ActiveTurnID != scope.TurnID.String() {
		t.Fatalf("retrieve Session = %+v", status)
	}
	var turn workerapi.SessionTurnResponse
	f.workerCall(t, "/run/turns/retrieve", workerapi.TurnReferenceRequest{SessionReferenceRequest: reference(self), TurnID: scope.TurnID.String()}, &turn)
	if turn.Completed == nil || turn.Completed.ID != scope.TurnID.String() || turn.Completed.Status != "running" {
		t.Fatalf("retrieve Turn = %+v", turn)
	}
	var events workerapi.ReadSessionEventsResponse
	f.workerCall(t, "/run/sessions/events/read-page", workerapi.ReadSessionEventsRequest{SessionReferenceRequest: reference(self), Limit: 1}, &events)
	if events.Completed == nil || len(events.Completed.Records) != 1 || !events.Completed.HasMore {
		t.Fatalf("read events = %+v", events)
	}

	outcome = workerOutcome{}
	f.workerCall(t, "/run/turns/messages/claim", workerapi.ClaimTurnMessageRequest{TurnExecutionRequest: f.turnCommand(scope), DeliveryID: uuid.NewV7().String()}, &outcome)
	if outcome.Failed == nil || outcome.Failed.Code != "turn_not_ready" {
		t.Fatalf("claim before readiness = %+v", outcome)
	}
	outcome = workerOutcome{}
	f.workerCall(t, "/run/turns/messages/ready", f.turnCommand(scope), &outcome)
	if !outcome.Accepted || outcome.Failed != nil {
		t.Fatalf("readiness = %+v", outcome)
	}
	var claimed workerapi.ClaimTurnMessageResponse
	f.workerCall(t, "/run/turns/messages/claim", workerapi.ClaimTurnMessageRequest{TurnExecutionRequest: f.turnCommand(scope), DeliveryID: uuid.NewV7().String()}, &claimed)
	if claimed.Delivery != nil || claimed.Failed != nil {
		t.Fatalf("claim without a message = %+v", claimed)
	}
	malformedTurn := f.turnCommand(scope)
	malformedTurn.TurnID = "not-a-turn"
	if w := f.worker(t, "/run/turns/settlement/begin", malformedTurn); w.Code != http.StatusInternalServerError {
		t.Fatalf("settlement with a malformed Turn = %d %s", w.Code, w.Body.String())
	}

	outcome = workerOutcome{}
	f.workerCall(t, "/run/sessions/output/write", workerapi.WriteSessionOutputRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunGeneration: scope.RunGeneration, Data: json.RawMessage(`1`)}, &outcome)
	if outcome.Failed == nil || outcome.Failed.Code != "turn_active" {
		t.Fatalf("Session output during a Turn = %+v", outcome)
	}
	output := f.outputRequest(scope)
	var written workerapi.WriteOutputResponse
	f.workerCall(t, "/run/turns/output/write", output, &written)
	if written.Completed == nil || written.Completed.Kind != "output" || written.Completed.TurnID == nil || *written.Completed.TurnID != scope.TurnID.String() || written.Completed.Provenance == nil || written.Completed.Provenance.RunID != f.RunID.String() {
		t.Fatalf("Turn output = %+v", written)
	}
	output.RunGeneration++
	written = workerapi.WriteOutputResponse{}
	f.workerCall(t, "/run/turns/output/write", output, &written)
	if written.Failed == nil || written.Failed.Code != "idempotency_conflict" {
		t.Fatalf("Turn output producer mismatch = %+v", written)
	}

	var control workerapi.SessionControlResponse
	f.workerCall(t, "/run/sessions/control", workerapi.SessionControlRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunGeneration: scope.RunGeneration}, &control)
	if control.TurnID == nil || *control.TurnID != scope.TurnID.String() || control.HoldID != nil {
		t.Fatalf("Session control = %+v", control)
	}

	var interrupted workerapi.InterruptSessionTurnResponse
	f.workerCall(t, "/run/turns/interrupt", workerapi.InterruptSessionTurnRequest{TurnReferenceRequest: workerapi.TurnReferenceRequest{SessionReferenceRequest: reference(self), TurnID: scope.TurnID.String()}}, &interrupted)
	if interrupted.Completed == nil || interrupted.Completed.SessionID != self || interrupted.Completed.HoldID == "" {
		t.Fatalf("interrupt = %+v", interrupted)
	}
	commit := workerapi.CommitActorTurnRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration, Disposition: "completed", Result: json.RawMessage(`{"answer":42}`), TargetInputSequence: 1}
	if w := f.worker(t, "/run/sessions/turns/commit", commit); w.Code != http.StatusConflict {
		t.Fatalf("stopped settlement = %d %s", w.Code, w.Body.String())
	}
	var resumed workerapi.ResumeSessionResponse
	f.workerCall(t, "/run/sessions/resume", workerapi.ResumeSessionRequest{SessionReferenceRequest: reference(self), HoldID: interrupted.Completed.HoldID}, &resumed)
	if resumed.Failed == nil || resumed.Failed.Code != "session_held" {
		t.Fatalf("resume = %+v", resumed)
	}
	var hold string
	if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id::text FROM sessions WHERE id=$1`, f.SessionID).Scan(&hold); err != nil || hold != interrupted.Completed.HoldID {
		t.Fatalf("hold=%s err=%v", hold, err)
	}
	var cancelled workerapi.CancelSessionResponse
	f.workerCall(t, "/run/sessions/cancel", workerapi.CancelSessionRequest{SessionReferenceRequest: reference(missing)}, &cancelled)
	if cancelled.Failed == nil || cancelled.Failed.Code != "session_not_found" {
		t.Fatalf("cancel missing Session = %+v", cancelled)
	}
	var closed workerapi.CloseSessionResponse
	f.workerCall(t, "/run/sessions/close", workerapi.CloseSessionRequest{SessionReferenceRequest: reference(self)}, &closed)
	if closed.Failed == nil || closed.Failed.Code != "session_held" {
		t.Fatalf("close from a held Session = %+v", closed)
	}
}
