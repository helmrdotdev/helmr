package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentTurnDispatchUsesCurrentAttachment(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	attachment, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentTurnRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence, AuthorityGeneration: attachment.AuthorityGeneration}
	if next, err := client.NextAgentTurn(t.Context(), request); err != nil || next != nil {
		t.Fatalf("empty queue: %+v %v", next, err)
	}
	admission, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "dispatch", Input: json.RawMessage(`[{"type":"text","text":"{\"message\":\"hello\"}"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []string{"attachment", "generation", "process", "lease"} {
		invalid := request
		switch wrong {
		case "attachment":
			invalid.AttachmentSequence++
		case "generation":
			invalid.AuthorityGeneration++
		case "process":
			invalid.Session.ProcessEpoch++
		case "lease":
			invalid.Session.ComputerLeaseEpoch++
		}
		if next, err := client.NextAgentTurn(t.Context(), invalid); !httpclient.IsStatus(err, http.StatusConflict) || next != nil {
			t.Fatalf("%s admitted: %+v %v", wrong, next, err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,gen_random_uuid(),$2,'local','test')`, f.Environment, f.Session)
	if next, err := client.NextAgentTurn(t.Context(), request); err != nil || next != nil {
		t.Fatalf("held queue: %+v %v", next, err)
	}
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, admission.TurnID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("denied dispatch mutated Turn: %s %v", status, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_holds SET released_at=clock_timestamp() WHERE session_id=$1`, f.Session)
	first, err := client.NextAgentTurn(t.Context(), request)
	if err != nil || first == nil {
		t.Fatalf("dispatch: %+v %v", first, err)
	}
	if first.TurnID != admission.TurnID.String() || first.Sequence != admission.Sequence || first.CreatedAt.IsZero() || first.Source.Kind != "user" {
		t.Fatalf("incorrect dispatch metadata: %+v", first)
	}
	second, err := client.NextAgentTurn(t.Context(), request)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("replay: %+v %v", second, err)
	}
	newAttachment, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.NextAgentTurn(t.Context(), request); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("retired attachment: %v", err)
	}
	request.AttachmentSequence = newAttachment.AttachmentSequence
	second, err = client.NextAgentTurn(t.Context(), request)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("reattachment: %+v %v", second, err)
	}
	receipt := workerapi.AgentTurnReceipt{Session: session, AttachmentSequence: request.AttachmentSequence, TurnID: first.TurnID, Outcome: json.RawMessage(`{"status":"cancelled"}`)}
	if _, err := client.ObserveAgentTurn(t.Context(), receipt); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("nonterminal Turn acknowledged: %v", err)
	}
	if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ack, err := client.ObserveAgentTurn(t.Context(), receipt)
		if err != nil || ack.Sequence != first.Sequence {
			t.Fatalf("retained terminal receipt: %+v %v", ack, err)
		}
	}
	for _, wrong := range []string{"outcome", "attachment", "process", "lease"} {
		invalid := receipt
		switch wrong {
		case "outcome":
			invalid.Outcome = json.RawMessage(`{"status":"completed","result":7}`)
		case "attachment":
			invalid.AttachmentSequence--
		case "process":
			invalid.Session.ProcessEpoch++
		case "lease":
			invalid.Session.ComputerLeaseEpoch++
		}
		if _, err := client.ObserveAgentTurn(t.Context(), invalid); !httpclient.IsStatus(err, http.StatusConflict) {
			t.Fatalf("%s terminal receipt accepted: %v", wrong, err)
		}
	}
}
