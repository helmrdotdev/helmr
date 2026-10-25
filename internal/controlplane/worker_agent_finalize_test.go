package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentFinalizeCompletesBeforeSaveAndReplaysResult(t *testing.T) {
	f := sessiontest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	sessionID := runtimeTestSession(f)
	a, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.Caller{Kind: "user", ID: f.User}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "finalize", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := client.AcquireAgentAttachment(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeTestAgentControl(t, client, sessionID, attachment.AttachmentSequence)
	if _, err := client.NextAgentTurn(t.Context(), workerapi.AgentTurnRequest{Session: sessionID, AttachmentSequence: attachment.AttachmentSequence, AuthorityGeneration: attachment.AuthorityGeneration}); err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentOperationRequest{Session: sessionID, RequestID: "close", AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String(), Method: workerapi.AgentMethodCloseProcessing, Payload: json.RawMessage(`null`)}
	closed, err := client.AgentOperation(t.Context(), request)
	if err != nil || closed.Error != nil || string(closed.Value) != "null" {
		t.Fatalf("close processing: %+v %v", closed, err)
	}
	request.RequestID, request.Method, request.Payload, request.DrainEvidence = "finalize", workerapi.AgentMethodFinalize, json.RawMessage(`{"result":{"saved":true}}`), "native scopes joined"
	first, err := client.AgentOperation(t.Context(), request)
	if err != nil || first.Error != nil {
		t.Fatalf("immediate completion %+v %v", first, err)
	}
	var valid bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT t.status='completed' AND s.status='requested' FROM turns t JOIN computer_saves s ON (s.environment_id,s.turn_id)=(t.environment_id,t.id) WHERE t.id=$1`, a.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("completion/save commit %v %v", valid, err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.Caller{Kind: "user", ID: f.User}, session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", IdempotencyKey: "later-cancel"}); err != nil {
		t.Fatal(err)
	}
	// Discard the first reply and repeat after a later Session control. The
	// original acknowledged result cannot be retracted or create another save.
	response, err := client.AgentOperation(t.Context(), request)
	if err != nil || response.Error != nil || string(response.Value) != string(first.Value) {
		t.Fatalf("retained completion %+v %v", response, err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(response.Value, &value); err != nil || string(value["status"]) != `"completed"` || string(value["result"]) != `{"saved":true}` {
		t.Fatalf("terminal result %s %v", response.Value, err)
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
		} else {
			var detail *httpclient.Error
			if errors.As(err, &detail) && detail.Code == workerapi.TurnReceiptsPending {
				t.Fatal("deadline was misclassified as pending callback receipts")
			}
		}
	})
}

func TestWorkerAgentFinalizeReportsOnlyPendingReceiptsAsRetryable(t *testing.T) {
	f := sessiontest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	id := runtimeTestSession(f)
	a, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.Caller{Kind: "user", ID: f.User}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "receipts", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := client.AcquireAgentAttachment(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeTestAgentControl(t, client, id, attachment.AttachmentSequence)
	if _, err := client.NextAgentTurn(t.Context(), workerapi.AgentTurnRequest{Session: id, AttachmentSequence: attachment.AttachmentSequence, AuthorityGeneration: attachment.AuthorityGeneration}); err != nil {
		t.Fatal(err)
	}
	askPayload, _ := json.Marshal(map[string]any{"creationId": uuid.NewV7(), "question": json.RawMessage(`{"type":"text","prompt":[{"type":"text","text":"Pending callback"}]}`)})
	request := workerapi.AgentOperationRequest{Session: id, RequestID: "ask", AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String(), Method: workerapi.AgentMethodAsk, Payload: askPayload}
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error != nil {
		t.Fatalf("pending ask %+v %v", response, err)
	}
	request.RequestID, request.Method, request.Payload = "close", workerapi.AgentMethodCloseProcessing, json.RawMessage(`null`)
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error != nil {
		t.Fatalf("close %+v %v", response, err)
	}
	request.RequestID, request.Method, request.Payload, request.DrainEvidence = "finalize", workerapi.AgentMethodFinalize, json.RawMessage(`{"result":1}`), "native scopes joined"
	_, err = client.AgentOperation(t.Context(), request)
	var detail *httpclient.Error
	if !errors.As(err, &detail) || detail.StatusCode != http.StatusServiceUnavailable || detail.Code != workerapi.TurnReceiptsPending {
		t.Fatalf("pending receipt classification: %v", err)
	}
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='running' AND result_digest IS NULL AND NOT EXISTS(SELECT 1 FROM computer_saves WHERE turn_id=$1) FROM turns WHERE id=$1`, a.TurnID).Scan(&pending); err != nil || !pending {
		t.Fatalf("pending receipts completed result: %v %v", pending, err)
	}
}
