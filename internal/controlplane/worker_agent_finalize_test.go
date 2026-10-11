package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentFinalizeRetainsPendingSaveAcrossLostReply(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	a, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "finalize", Input: json.RawMessage(`[]`)})
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
	closed, err := client.AgentOperation(t.Context(), request)
	if err != nil || closed.Error != nil || string(closed.Value) != "null" {
		t.Fatalf("close processing: %+v %v", closed, err)
	}
	request.RequestID, request.Method, request.Payload, request.DrainEvidence = "finalize", int32(agentv1.Operation_METHOD_FINALIZE), json.RawMessage(`{"result":{"saved":true}}`), "native scopes joined"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.AgentOperation(ctx, request); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var finalizing bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT status='finalizing' FROM turns WHERE id=$1`, a.TurnID).Scan(&finalizing); err != nil {
			t.Fatal(err)
		}
		if finalizing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finalization did not record own save")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("pending save settled operation: %v", err)
	default:
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("lost reply was reported as a final outcome")
	}
	if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	response, err := client.AgentOperation(t.Context(), request)
	if err != nil || response.Error != nil {
		t.Fatalf("retained finalization: %+v %v", response, err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(response.Value, &value); err != nil || string(value["status"]) != `"cancelled"` || value["result"] != nil {
		t.Fatalf("terminal winner changed: %s %v", response.Value, err)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_saves WHERE turn_id=$1`, a.TurnID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry created another save: %d %v", count, err)
	}
	t.Run("database wait remains transient", func(t *testing.T) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(t.Context(), `SELECT id FROM sessions WHERE id=$1 FOR UPDATE`, f.Session); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		if _, err := client.AgentOperation(ctx, request); !httpclient.IsStatus(err, http.StatusServiceUnavailable) {
			t.Fatalf("database wait escaped finalization deadline: %v", err)
		}
	})
}
