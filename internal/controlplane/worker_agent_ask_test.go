package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentAskHTTPAndSDK(t *testing.T) {
	f := agenttest.New(t)
	var cfg ServerConfig
	handler := newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}; cfg = *c })
	server := httptest.NewServer(handler)
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	caller := agent.Caller{Kind: "user", ID: f.User}
	admission, err := agent.Enqueue(t.Context(), f.Pool, caller, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "ask", Input: json.RawMessage(`[]`)})
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
	request := workerapi.AgentOperationRequest{Session: session, RequestID: "ask", AuthorityGeneration: attachment.AuthorityGeneration, TurnID: admission.TurnID.String()}
	operation := func(method agentv1.Operation_Method, payload any) workerapi.AgentOperationResponse {
		t.Helper()
		request.Method = int32(method)
		request.Payload, _ = json.Marshal(payload)
		result, err := client.AgentOperation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	q := json.RawMessage(`{"prompt":[{"type":"text","text":"keep\u0000雪"}],"answer":{"type":"text"}}`)
	ids := []uuid.UUID{uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}
	for _, id := range ids {
		result := operation(agentv1.Operation_METHOD_ASK, map[string]any{"creationId": id, "question": q})
		if result.Error != nil {
			t.Fatal(result.Error)
		}
	}
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	queries := db.New(f.Pool)
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	hf := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE org_members SET role='owner' WHERE user_id=$1`, f.User); err != nil {
		t.Fatal(err)
	}
	issuer := auth.Principal{OrgID: org, UserID: f.User, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}
	scope := auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}
	issue := func(name string, permissions ...auth.Permission) (string, uuid.UUID) {
		t.Helper()
		key, err := identity.IssueAPIKey(t.Context(), queries, issuer, scope, identity.APIKeyInput{Name: name, Permissions: permissions})
		if err != nil {
			t.Fatal(err)
		}
		var id uuid.UUID
		if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM api_keys WHERE token_hash=$1`, auth.HashAPIKey(key.Raw)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return key.Raw, id
	}
	reader, _ := issue("reader", auth.PermissionSessionsRead)
	responder, responderID := issue("responder", auth.PermissionAsksRespond)
	both, _ := issue("sdk", auth.PermissionSessionsRead, auth.PermissionAsksRespond)
	base := fmt.Sprintf("/v1/sessions/%s/turns/%s/asks", f.Session, admission.TurnID)
	call := func(method, path, token, body string, want int) map[string]json.RawMessage {
		t.Helper()
		response := hf.request(t, method, path, token, body)
		if response.Code != want {
			t.Fatalf("%s %s: %d %s want %d", method, path, response.Code, response.Body, want)
		}
		var value map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	exact := base + "/" + ids[0].String()
	call(http.MethodGet, exact, responder, "", http.StatusForbidden)
	call(http.MethodPost, exact+"/respond", reader, `{"answer":null,"response_id":"answer"}`, http.StatusForbidden)
	for _, body := range []string{`{"response_id":"answer"}`, `{"answer":null}`, `{"answer":null,"response_id":"answer","responded_by_user_id":"fake"}`, `{"answer":null,"answer":false,"response_id":"answer"}`} {
		call(http.MethodPost, exact+"/respond", responder, body, http.StatusBadRequest)
	}
	for _, body := range []string{`{"answer":"\ud800","response_id":"invalid"}`} {
		failure := call(http.MethodPost, exact+"/respond", responder, body, http.StatusUnprocessableEntity)
		if !bytes.Contains(failure["error"], []byte(`"answer_invalid"`)) {
			t.Fatalf("answer classification: %s", failure["error"])
		}
	}
	choiceID := uuid.NewV7()
	choiceQuestion := json.RawMessage(`{"prompt":[{"type":"text","text":"Choose a value"}],"answer":{"type":"choice","options":[{"id":"one","label":"One","value":{"same":2}}]}}`)
	if result := operation(agentv1.Operation_METHOD_ASK, map[string]any{"creationId": choiceID, "question": choiceQuestion}); result.Error != nil {
		t.Fatal(result.Error)
	}
	choicePath := base + "/" + choiceID.String()
	duplicate := call(http.MethodPost, choicePath+"/respond", responder, `{"answer":{"selected":[{"id":"one","value":{"same":1,"same":2}}]},"response_id":"choice"}`, http.StatusUnprocessableEntity)
	if !bytes.Contains(duplicate["error"], []byte(`"answer_invalid"`)) {
		t.Fatalf("duplicate option value classification: %s", duplicate["error"])
	}
	call(http.MethodPost, choicePath+"/respond", responder, `{"answer":{"selected":[{"id":"one","value":{"same":2}}]},"response_id":"choice"}`, http.StatusOK)
	pending := call(http.MethodGet, exact, reader, "", http.StatusOK)
	if string(pending["status"]) != `"pending"` {
		t.Fatal("invalid answer settled ask")
	}
	for _, body := range []string{`{"answer":null,"response_id":"\ud800"}`, `{"answer":null,"response_id":"one","response_id":"two"}`, `{"answer":null,"response_id":"one","\ud800":0}`, `{"ANSWER":null,"response_id":"one"}`, `{"answer":null,"ANSWER":false,"response_id":"one"}`} {
		call(http.MethodPost, exact+"/respond", responder, body, http.StatusBadRequest)
	}
	body := `{"answer":"","response_id":"answer"}`
	receipt := call(http.MethodPost, exact+"/respond", responder, body, http.StatusOK)
	if receipt["prompt"] != nil || receipt["answer_control"] != nil || string(receipt["answer"]) != `""` || string(receipt["responded_by_api_key_id"]) != `"`+responderID.String()+`"` {
		t.Fatalf("answer receipt %v", receipt)
	}
	observed := operation(agentv1.Operation_METHOD_WAIT_ASK, map[string]any{"askId": ids[0]})
	var resolved struct {
		Answer      json.RawMessage           `json:"answer"`
		RespondedBy struct{ Kind, ID string } `json:"respondedBy"`
	}
	if err := json.Unmarshal(observed.Value, &resolved); err != nil || observed.Error != nil || string(resolved.Answer) != `""` || resolved.RespondedBy.Kind != "api_key" || resolved.RespondedBy.ID != responderID.String() {
		t.Fatalf("runtime answer %+v %v", observed, err)
	}
	if result := operation(agentv1.Operation_METHOD_WITHDRAW_ASK, map[string]any{"askId": ids[0]}); result.Error != nil {
		t.Fatal(result.Error)
	}
	// Human Viewer answers use membership, independently of service role rules.
	if _, err := f.Pool.Exec(t.Context(), `UPDATE org_members SET role='viewer' WHERE user_id=$1`, f.User); err != nil {
		t.Fatal(err)
	}
	login := hf.session(t, f.User, org)
	humanPath := fmt.Sprintf("/api/projects/%s/environments/%s/sessions/%s/turns/%s/asks/%s", project, f.Environment, f.Session, admission.TurnID, ids[1])
	call(http.MethodGet, humanPath, login, "", http.StatusOK)
	human := call(http.MethodPost, humanPath+"/respond", login, `{"answer":"Allow","response_id":"human"}`, http.StatusOK)
	if string(human["responded_by_user_id"]) != `"`+f.User.String()+`"` {
		t.Fatal("missing human attribution")
	}
	// The actual SDK traverses the mounted API, then submits the third answer.
	script, err := filepath.Abs("../../sdk/typescript/testdata/asks-http.ts")
	if err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"url": server.URL, "apiKey": both, "sessionId": f.Session.String(), "turnId": admission.TurnID.String(), "askId": ids[2].String()})
	command := exec.CommandContext(t.Context(), "bun", script)
	command.Stdin = bytes.NewReader(config)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ask SDK composition: %v\n%s", err, output)
	}
	assertAskCLIHTTP(t, server.URL, both, f.Session.String(), admission.TurnID.String(), ids[3].String(), ids[4].String())
	// Exact containment is checked regardless of a valid ask UUID.
	call(http.MethodGet, strings.Replace(exact, admission.TurnID.String(), uuid.NewV7().String(), 1), reader, "", http.StatusForbidden)
	if _, err := f.Pool.Exec(t.Context(), `UPDATE turn_asks SET question=NULL,answer=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	expired := call(http.MethodGet, exact, reader, "", http.StatusOK)
	if string(expired["payload_expired"]) != "true" || expired["answer"] != nil || expired["prompt"] != nil || expired["answer_control"] != nil || expired["responded_by_api_key_id"] == nil {
		t.Fatalf("expired payload %v", expired)
	}
}

func TestWorkerAgentAskObservationsReleaseCapacity(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	a, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "many asks", Input: json.RawMessage(`[]`)})
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ids := make([]uuid.UUID, 65)
	req := workerapi.AgentOperationRequest{Session: session, AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String()}
	for i := range ids {
		ids[i] = uuid.NewV7()
		req.RequestID = fmt.Sprintf("create-%d", i)
		req.Method = int32(agentv1.Operation_METHOD_ASK)
		req.Payload, _ = json.Marshal(map[string]any{"creationId": ids[i], "question": json.RawMessage(`{"prompt":[],"answer":{"type":"text"}}`)})
		result, err := client.AgentOperation(ctx, req)
		if err != nil || result.Error != nil {
			t.Fatalf("create %+v %v", result, err)
		}
	}
	errs := make(chan error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			r := req
			r.RequestID = fmt.Sprintf("observe-%d", i)
			r.Method = int32(agentv1.Operation_METHOD_WAIT_ASK)
			r.Payload, _ = json.Marshal(map[string]any{"askId": id})
			result, err := client.AgentOperation(ctx, r)
			if err == nil && (result.Error != nil || string(result.Value) != "null") {
				err = fmt.Errorf("pending observation %+v", result)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	req.RequestID = "withdraw"
	req.Method = int32(agentv1.Operation_METHOD_WITHDRAW_ASK)
	req.Payload, _ = json.Marshal(map[string]any{"askId": ids[0]})
	if result, err := client.AgentOperation(ctx, req); err != nil || result.Error != nil {
		t.Fatalf("withdraw %+v %v", result, err)
	}
	req.RequestID = "observe-cancelled"
	req.Method = int32(agentv1.Operation_METHOD_WAIT_ASK)
	if result, err := client.AgentOperation(ctx, req); err != nil || result.Error == nil || result.Error.Code != "ask_cancelled" {
		t.Fatalf("cancel observation %+v %v", result, err)
	}
}
