package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// actorExecutionHTTP serves the control plane built by NewServer over an
// Actor execution's database. The execution's worker authenticates with an
// epoch token exchanged for its seeded host credential; login sessions
// authenticate as in production and API key bearer tokens as the principal
// registered for them.
type actorExecutionHTTP struct {
	*sessiontest.Execution
	httpPostgresFixture
	principals  *principalAuthenticator
	workerToken string
}

// newActorExecution builds an Actor execution whose Session holds the input
// as its first enqueued Turn, when there is one, and whose lease the worker
// claimed, started and entered when start is set, and serves it.
func newActorExecution(t *testing.T, input json.RawMessage, start bool) *actorExecutionHTTP {
	t.Helper()
	f := sessiontest.NewExecution(t)
	if input != nil {
		if _, err := session.ApplyAdmission(t.Context(), f.Pool, session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.SessionID}, Mode: session.EnqueueOnly, Data: input}); err != nil {
			t.Fatal(err)
		}
	}
	if start {
		f.ClaimAndStart(t)
	}
	keys, err := auth.NewKeys(testAuthRootKey())
	if err != nil {
		t.Fatal(err)
	}
	principals := &principalAuthenticator{principals: map[string]auth.Principal{}}
	handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.Auth = principals
		cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	})
	return &actorExecutionHTTP{
		Execution:           f,
		httpPostgresFixture: httpPostgresFixture{pool: f.Pool, queries: db.New(f.Pool), handler: handler, keys: keys},
		principals:          principals,
		workerToken:         seedHostCredential(t, f.Pool, f.WorkerID).token(t, handler),
	}
}

// fence is the worker's receipt for its claimed lease.
func (f *actorExecutionHTTP) fence() workerapi.RunLeaseFence {
	lease := f.Claim.Lease()
	return workerapi.RunLeaseFence{ID: pgvalue.UUIDString(lease.ID), LeaseSequence: lease.LeaseSequence}
}

// apiKey registers an API key bearer token that authenticates as principal.
func (f *actorExecutionHTTP) apiKey(principal auth.Principal) string {
	return f.principals.apiKey(principal)
}

// worker posts the body to the worker route as the execution's worker.
func (f *actorExecutionHTTP) worker(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.serveWorker(t, recorder, path, body)
	return recorder
}

// serveWorker posts the body to the worker route as the execution's worker
// and writes the response to w.
func (f *actorExecutionHTTP) serveWorker(t *testing.T, w http.ResponseWriter, path string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1"+path, strings.NewReader(string(raw)))
	request.Header.Set("Authorization", "Bearer "+f.workerToken)
	request.Header.Set("Content-Type", "application/json")
	f.handler.ServeHTTP(w, request)
}

// workerCall posts the body to the worker route, requires success and
// decodes the response into result when it is not nil.
func (f *actorExecutionHTTP) workerCall(t *testing.T, path string, body any, result any) {
	t.Helper()
	w := f.worker(t, path, body)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("worker request %s: status=%d body=%s", path, w.Code, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatal(err)
		}
	}
}

// receiveTurn activates the Turn at the input sequence, registering the
// worker's Actor input wait after the previous sequence while it is queued,
// and returns the Turn's producer scope.
func (f *actorExecutionHTTP) receiveTurn(t *testing.T, sequence int64) run.TurnScope {
	t.Helper()
	params := db.GetSessionTurnAtSequenceForUpdateParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SessionID: pgvalue.UUID(f.SessionID), Sequence: sequence}
	input, err := f.queries.GetSessionTurnAtSequenceForUpdate(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if input.Status == "queued" {
		after := sequence - 1
		waitParams, _ := json.Marshal(workerActorInputWaitParams{SessionID: f.SessionID.String(), AfterInputSequence: after})
		f.workerCall(t, "/run/waits/create", workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: waitParams, ActorSpeculativeInputSequence: &after}, nil)
		input, err = f.queries.GetSessionTurnAtSequenceForUpdate(t.Context(), params)
		if err != nil {
			t.Fatal(err)
		}
	}
	if input.Status != "running" || !input.RunGeneration.Valid {
		t.Fatalf("input was not activated: %+v", input)
	}
	return run.TurnScope{EnvironmentID: f.EnvironmentID, SessionID: f.SessionID, TurnID: pgvalue.MustUUIDValue(input.ID), RunID: f.RunID, AttemptNumber: input.AttemptNumber.Int32, RunGeneration: input.RunGeneration.Int64}
}

// turnCommand addresses the scope's Turn work with a new correlation.
func (f *actorExecutionHTTP) turnCommand(scope run.TurnScope) workerapi.TurnExecutionRequest {
	return workerapi.TurnExecutionRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration}
}

// turnCommitRequest begins the Turn's settlement and returns the worker's
// commit of a completed result at the first input sequence.
func (f *actorExecutionHTTP) turnCommitRequest(t *testing.T, scope run.TurnScope) workerapi.CommitActorTurnRequest {
	t.Helper()
	f.workerCall(t, "/run/turns/settlement/begin", f.turnCommand(scope), nil)
	return workerapi.CommitActorTurnRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration, Disposition: "completed", Result: json.RawMessage(`{"answer":42}`), TargetInputSequence: 1}
}

// outputRequest is the worker's keyed output on the scope's Turn.
func (f *actorExecutionHTTP) outputRequest(scope run.TurnScope) workerapi.WriteTurnOutputRequest {
	return workerapi.WriteTurnOutputRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration, Data: json.RawMessage(`{"type":"permission_granted","requestId":"native-1","actionBinding":"command-1"}`), IdempotencyKey: "permission-1"}
}

// interruptTurn interrupts the Turn through the public Session operation and
// returns a committed rejection as its receipt.
func (f *actorExecutionHTTP) interruptTurn(t *testing.T, scope run.TurnScope, key string) session.ControlReceipt {
	t.Helper()
	receipt, err := session.ApplyInterrupt(t.Context(), f.Pool, session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: scope.EnvironmentID, SessionID: scope.SessionID}, IdempotencyKey: key}, TurnID: scope.TurnID})
	if err != nil {
		t.Fatalf("interrupt: %+v %v", receipt, err)
	}
	return receipt
}
