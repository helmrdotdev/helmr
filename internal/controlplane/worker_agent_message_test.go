package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/identity"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"
)

func TestWorkerAgentMessageReceiptSurvivesReattachment(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	a, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, Input: json.RawMessage(`[]`)})
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
	op := workerapi.AgentOperationRequest{Session: session, RequestID: "register", AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String(), Method: int32(agentv1.Operation_METHOD_REGISTER_MESSAGES), Payload: json.RawMessage(`null`)}
	if response, err := client.AgentOperation(t.Context(), op); err != nil || response.Error != nil {
		t.Fatalf("register: %+v %v", response, err)
	}
	parent := f
	parent.Session = uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) VALUES('until_environment_deletion',$1,$2,$3,$4,$5,$2,0);
 INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$2,1,$5,1,'ready');
 UPDATE sessions SET parent_session_id=$2,requester_session_id=$2,root_session_id=$2,causal_depth=1 WHERE environment_id=$1 AND id=$6`, pgx.QueryExecModeSimpleProtocol, f.Environment, parent.Session, f.Agent, f.Deployment, f.Computer, f.Session)
	sendOp := op
	sendOp.Session = runtimeTestSession(parent)
	sendOp.TurnID = ""
	sendOp.Method, sendOp.RequestID = int32(agentv1.Operation_METHOD_RUNTIME_MCP), "send"
	sendOp.Payload, _ = json.Marshal(map[string]any{"tool": "send_turn", "arguments": map[string]any{"sessionId": f.Session.String(), "turnId": a.TurnID.String(), "message": json.RawMessage(`[{"type":"text","text":"steer"}]`), "idempotencyKey": "parent-steer"}})
	sent, err := client.AgentOperation(t.Context(), sendOp)
	if err != nil || sent.Error != nil {
		t.Fatalf("send: %+v error=%+v %v", sent, sent.Error, err)
	}
	if again, err := client.AgentOperation(t.Context(), sendOp); err != nil || again.Error != nil || string(again.Value) != string(sent.Value) {
		t.Fatalf("send retry: %+v %v", again, err)
	}
	request := workerapi.AgentMessageRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence, AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String()}
	next, err := client.NextAgentMessage(t.Context(), request)
	if err != nil || next == nil || string(next.Input) != `[{"text":"steer","type":"text"}]` {
		t.Fatalf("claim: %+v %v", next, err)
	}

	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE org_members SET role='owner' WHERE user_id=$1`, f.User); err != nil {
		t.Fatal(err)
	}
	key, err := identity.IssueAPIKey(t.Context(), db.New(f.Pool), auth.Principal{OrgID: org, UserID: f.User, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}, identity.APIKeyInput{Name: "SDK messages", Permissions: []auth.Permission{auth.PermissionSessionsSend}})
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../sdk/typescript/testdata/messages-http.ts")
	if err != nil {
		t.Fatal(err)
	}
	runSDK := func(phase string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"url": server.URL, "apiKey": key.Raw, "sessionId": f.Session.String(), "turnId": a.TurnID.String(), "phase": phase})
		command := exec.CommandContext(t.Context(), "bun", script)
		command.Stdin = bytes.NewReader(raw)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("SDK messages %s: %v\n%s", phase, err, output)
		}
	}
	runSDK("live")
	receipt := workerapi.AgentMessageReceipt{Session: session, AttachmentSequence: attachment.AttachmentSequence, TurnID: next.TurnID, MessageID: next.MessageID}
	wrong := receipt
	wrong.TurnID = uuid.NewV7().String()
	if err := client.ObserveAgentMessage(t.Context(), wrong); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("wrong target: %v", err)
	}
	attachment, err = client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ObserveAgentMessage(t.Context(), receipt); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("stale attachment: %v", err)
	}
	request.AttachmentSequence = attachment.AttachmentSequence
	repeated, err := client.NextAgentMessage(t.Context(), request)
	if err != nil || repeated == nil || repeated.MessageID != next.MessageID {
		t.Fatalf("reattachment changed callback: %+v %v", repeated, err)
	}
	op.Method, op.Payload = int32(agentv1.Operation_METHOD_CLOSE_PROCESSING), json.RawMessage(`null`)
	if response, err := client.AgentOperation(t.Context(), op); err != nil || response.Error != nil {
		t.Fatalf("close: %+v %v", response, err)
	}
	runSDK("closed")
	op.Method, op.Payload, op.DrainEvidence = int32(agentv1.Operation_METHOD_FINALIZE), json.RawMessage(`{"result":null}`), "joined"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		response, err := client.AgentOperation(ctx, op)
		if err == nil && response.Error != nil {
			err = fmt.Errorf("%+v", response.Error)
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("pending callback settled finalization: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	receipt.AttachmentSequence = attachment.AttachmentSequence
	for range 2 {
		if err := client.ObserveAgentMessage(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
	}
	if next, err := client.NextAgentMessage(t.Context(), request); err != nil || next != nil {
		t.Fatalf("completed replay: %+v %v", next, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		var state string
		if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, a.TurnID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "finalizing" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("finalize did not proceed: %s", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}
