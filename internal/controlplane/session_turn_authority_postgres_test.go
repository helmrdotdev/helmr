package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type delayedTurnOutputWriter struct {
	http.ResponseWriter
	reached chan struct{}
	release chan struct{}
	drop    bool
}

func (w delayedTurnOutputWriter) Write(b []byte) (int, error) {
	close(w.reached)
	<-w.release
	if w.drop {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(b)
}

// A Turn output whose response is delayed or lost after it committed is not
// admitted again once the Turn stops: the retry fails as turn_stopping.
func TestSessionTurnDelayedOutputResponsePostgres(t *testing.T) {
	for _, drop := range []bool{false, true} {
		name := "delayed"
		if drop {
			name = "lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorExecution(t, json.RawMessage(`{"sequence":1}`), true)
			scope := f.receiveTurn(t, 1)
			req := f.outputRequest(scope)
			reached, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			recorder := httptest.NewRecorder()
			go func() {
				f.serveWorker(t, delayedTurnOutputWriter{ResponseWriter: recorder, reached: reached, release: release, drop: drop}, "/run/turns/output/write", req)
				close(done)
			}()
			<-reached
			if receipt := f.interruptTurn(t, scope, "stop"); receipt.Status != "accepted" {
				t.Fatalf("stop: %+v", receipt)
			}
			close(release)
			<-done
			if !drop {
				var r workerapi.WriteOutputResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &r); err != nil || r.Completed == nil {
					t.Fatalf("delayed receipt: %+v %v", r, err)
				}
			}
			var retry workerapi.WriteOutputResponse
			f.workerCall(t, "/run/turns/output/write", req, &retry)
			if retry.Failed == nil || retry.Failed.Code != "turn_stopping" {
				t.Fatalf("uncertain retry after stop: %+v", retry)
			}
			var hold string
			var terminals int
			if err := f.Pool.QueryRow(t.Context(), `SELECT s.dispatch_hold_reason,(SELECT count(*) FROM session_events e WHERE e.session_id=s.id AND e.kind IN ('turn.completed','turn.failed')) FROM sessions s WHERE s.id=$1`, f.SessionID).Scan(&hold, &terminals); err != nil || hold != "interrupt_requested" || terminals != 0 {
				t.Fatalf("stop falsely converged or committed: %s %d %v", hold, terminals, err)
			}
		})
	}
}

func TestSessionTurnCompletionResultPresencePostgres(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "absent"
		if present {
			name = "null"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorExecution(t, json.RawMessage(`{"sequence":1}`), true)
			scope := f.receiveTurn(t, 1)
			req := f.turnCommitRequest(t, scope)
			req.Result = nil
			if present {
				req.Result = json.RawMessage(`null`)
			}
			// Exercise actual JSON encoding/decoding, including omitting an absent result.
			var response workerapi.CommitActorTurnResponse
			f.workerCall(t, "/run/sessions/turns/commit", req, &response)
			event, err := f.queries.GetSessionEvent(t.Context(), db.GetSessionEventParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SessionID: pgvalue.UUID(f.SessionID), ID: pgvalue.UUID(uuid.MustParse(response.EventID))})
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]json.RawMessage
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			value, ok := data["result"]
			if ok != present || (present && string(value) != "null") {
				t.Fatalf("result presence lost: %s", event.Data)
			}
			if len(data["computer_disk_version_id"]) != 0 || event.ComputerDiskVersionID.Valid || len(data["error"]) != 0 {
				t.Fatalf("terminal envelope: %s", event.Data)
			}
			if response.CorrelationID != req.CorrelationID || response.CommittedInputSequence != 1 || response.Lease != req.Lease {
				t.Fatalf("commit response: %+v", response)
			}
		})
	}
}
