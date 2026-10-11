package controlplane

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http/httptest"
	"testing"
)

func TestWorkerAgentFailureHasOneObservableOutcome(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	a, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "failure", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.NextAgentTurn(t.Context(), workerapi.AgentTurnRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence, AuthorityGeneration: attachment.AuthorityGeneration}); err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentOperationRequest{Session: session, RequestID: "close", AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String(), Method: int32(agentv1.Operation_METHOD_CLOSE_PROCESSING), Payload: json.RawMessage(`null`)}
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error != nil {
		t.Fatalf("close: %+v %v", response, err)
	}
	request.RequestID, request.Method, request.Payload = "failure", int32(agentv1.Operation_METHOD_FAIL), json.RawMessage(`{"error":{"code":"application_error","message":"bad\u0000input"}}`)
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error == nil {
		t.Fatalf("missing drainage accepted: %+v %v", response, err)
	}
	request.DrainEvidence = "native scopes joined"
	for range 2 {
		response, err := client.AgentOperation(t.Context(), request)
		if err != nil || response.Error != nil {
			t.Fatalf("failure: %+v %v", response, err)
		}
		ack, err := client.ObserveAgentTurn(t.Context(), workerapi.AgentTurnReceipt{Session: session, AttachmentSequence: attachment.AttachmentSequence, TurnID: a.TurnID.String(), Outcome: response.Value})
		if err != nil || ack.Sequence != a.Sequence {
			t.Fatalf("receipt: %+v %v", ack, err)
		}
	}
	view, err := agent.GetTurn(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, f.Environment, f.Session, a.TurnID)
	if err != nil || view.Error == nil || view.Error.Code != "application_error" {
		t.Fatalf("inspection: %+v %v", view, err)
	}
	wire := agentTurnResponse(view)
	if wire.Error == nil || wire.Error.Message == nil || *wire.Error.Message != "bad\x00input" {
		t.Fatalf("wire: %+v", wire)
	}
}

func TestWorkerAgentHoldBeforeTurn(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	attachment, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentOperationRequest{Session: session, RequestID: "hold", AuthorityGeneration: attachment.AuthorityGeneration, Method: int32(agentv1.Operation_METHOD_HOLD), Payload: json.RawMessage(`{"reason":"setup_failed"}`)}
	for range 2 {
		response, err := client.AgentOperation(t.Context(), request)
		if err != nil || response.Error != nil || string(response.Value) != "null" {
			t.Fatalf("hold without Turn: %+v %v", response, err)
		}
	}
	view, err := agent.GetSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, f.Environment, f.Session)
	if err != nil || len(view.Holds) != 1 || view.Holds[0].Scope != "local" || view.Holds[0].Reason != "setup_failed" {
		t.Fatalf("hold inspection: %+v %v", view, err)
	}
}
