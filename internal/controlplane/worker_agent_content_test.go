package controlplane

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentContentAndSuppressedResponse(t *testing.T) {
	f := sessiontest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	sessionID := runtimeTestSession(f)
	a, err := session.Enqueue(t.Context(), f.Pool, nil, session.Caller{Kind: "user", ID: f.User}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "content", Input: json.RawMessage(`[]`)})
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
	request := workerapi.AgentOperationRequest{Session: sessionID, RequestID: "output", AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String(), Method: workerapi.AgentMethodOutput}
	output := map[string]any{"outputId": uuid.NewV7().String(), "value": "progress\x00"}
	request.Payload, _ = json.Marshal(output)
	first, err := client.AgentOperation(t.Context(), request)
	if err != nil || first.Error != nil {
		t.Fatalf("output: %+v %v", first, err)
	}
	if replay, err := client.AgentOperation(t.Context(), request); err != nil || replay.Error != nil || string(first.Value) != string(replay.Value) {
		t.Fatalf("output replay: %+v %v", replay, err)
	}
	output["value"] = []map[string]string{{"type": "file", "file": "unsupported"}}
	request.Payload, _ = json.Marshal(output)
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error == nil || response.Error.Code != "content_kind_unsupported" {
		t.Fatalf("file admission: %+v %v", response, err)
	}
	request.Method = workerapi.AgentMethodRespond
	request.Payload, _ = json.Marshal(map[string]any{"responseId": uuid.NewV7().String(), "value": []any{}})
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error != nil || string(response.Value) != "null" {
		t.Fatalf("response: %+v %v", response, err)
	}
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error != nil {
		t.Fatalf("response replay: %+v %v", response, err)
	}
	request.Method, request.Payload = workerapi.AgentMethodCloseProcessing, json.RawMessage(`null`)
	if response, err := client.AgentOperation(t.Context(), request); err != nil || response.Error != nil {
		t.Fatalf("close: %+v %v", response, err)
	}
	request.Method, request.Payload, request.DrainEvidence = workerapi.AgentMethodFail, json.RawMessage(`{"error":{"code":"application_error","message":"failure"}}`), "joined"
	response, err := client.AgentOperation(t.Context(), request)
	if err != nil || response.Error != nil {
		t.Fatalf("failure: %+v %v", response, err)
	}
	var outcome map[string]json.RawMessage
	if err := json.Unmarshal(response.Value, &outcome); err != nil || outcome["response"] != nil || string(outcome["status"]) != `"failed"` {
		t.Fatalf("failed response publication %s %v", response.Value, err)
	}
	view, err := session.GetTurn(t.Context(), f.Pool, session.Caller{Kind: "user", ID: f.User}, f.Environment, f.Session, a.TurnID)
	if err != nil || view.Response != nil {
		t.Fatalf("failed response read %+v %v", view, err)
	}
	var data []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT data FROM session_events WHERE turn_id=$1 AND kind='turn.output'`, a.TurnID).Scan(&data); err != nil || string(data) != `[{"text":"progress\u0000","type":"text"}]` {
		t.Fatalf("progress lost %s: %v", data, err)
	}
}
