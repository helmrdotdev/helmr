package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
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

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentAskHTTPAndSDK(t *testing.T) {
	f := sessiontest.New(t)
	var cfg ServerConfig
	handler := newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}; cfg = *c })
	server := httptest.NewServer(handler)
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	sessionID := runtimeTestSession(f)
	caller := session.UserCaller(f.Principal(t))
	admission, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "ask", Input: json.RawMessage(`[]`)})
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
	request := workerapi.AgentOperationRequest{Session: sessionID, RequestID: "ask", AuthorityGeneration: attachment.AuthorityGeneration, TurnID: admission.TurnID.String()}
	operation := func(method workerapi.AgentOperationMethod, payload any) workerapi.AgentOperationResponse {
		t.Helper()
		request.Method = method
		request.Payload, _ = json.Marshal(payload)
		result, err := client.AgentOperation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	q := json.RawMessage(`{"prompt":[{"type":"text","text":"keep\u0000雪"}],"type":"text"}`)
	ids := []uuid.UUID{uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}
	for _, id := range ids {
		result := operation(workerapi.AgentMethodAsk, map[string]any{"creationId": id, "question": q})
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
	hf := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE org_members SET is_owner=true WHERE user_id=$1`, f.User); err != nil {
		t.Fatal(err)
	}
	issuer := f.Principal(t)
	scope := auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}
	issue := func(name string, permissions ...auth.Permission) (string, uuid.UUID) {
		t.Helper()
		key, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, issuer, scope, testAPIKeyInput{Name: name, RolePermissions: permissions})
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
	responder, responderID := issue("responder", auth.PermissionSessionsCreate, auth.PermissionSessionsRead, auth.PermissionSessionsAnswer)
	both, _ := issue("sdk", auth.PermissionSessionsCreate, auth.PermissionSessionsRead, auth.PermissionSessionsAnswer)
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
	call(http.MethodGet, exact, responder, "", http.StatusOK)
	call(http.MethodPost, exact+"/answer", reader, `{"answer":"","answer_id":"answer"}`, http.StatusForbidden)
	for _, body := range []string{`{"answer_id":"answer"}`, `{"answer":null}`, `{"answer":null,"answer_id":"answer","answered_by_user_id":"fake"}`, `{"answer":null,"answer":false,"answer_id":"answer"}`} {
		call(http.MethodPost, exact+"/answer", responder, body, http.StatusBadRequest)
	}
	for _, body := range []string{`{"answer":"\ud800","answer_id":"invalid"}`} {
		failure := call(http.MethodPost, exact+"/answer", responder, body, http.StatusUnprocessableEntity)
		if !bytes.Contains(failure["error"], []byte(`"answer_invalid"`)) {
			t.Fatalf("answer classification: %s", failure["error"])
		}
	}
	choiceID := uuid.NewV7()
	choiceQuestion := json.RawMessage(`{"prompt":[{"type":"text","text":"Choose a value"}],"type":"choice","options":[{"id":"one","label":"One","value":{"same":2}}]}`)
	if result := operation(workerapi.AgentMethodAsk, map[string]any{"creationId": choiceID, "question": choiceQuestion}); result.Error != nil {
		t.Fatal(result.Error)
	}
	choicePath := base + "/" + choiceID.String()
	duplicate := call(http.MethodPost, choicePath+"/answer", responder, `{"answer":{"selected":[{"id":"one","value":{"same":1,"same":2}}]},"answer_id":"choice"}`, http.StatusUnprocessableEntity)
	if !bytes.Contains(duplicate["error"], []byte(`"answer_invalid"`)) {
		t.Fatalf("duplicate option value classification: %s", duplicate["error"])
	}
	call(http.MethodPost, choicePath+"/answer", responder, `{"answer":{"selectedIds":["one"]},"answer_id":"choice"}`, http.StatusOK)
	pending := call(http.MethodGet, exact, reader, "", http.StatusOK)
	if string(pending["status"]) != `"pending"` {
		t.Fatal("invalid answer settled ask")
	}
	for _, body := range []string{`{"answer":null,"answer_id":"\ud800"}`, `{"answer":null,"answer_id":"one","answer_id":"two"}`, `{"answer":null,"answer_id":"one","\ud800":0}`, `{"ANSWER":null,"answer_id":"one"}`, `{"answer":null,"ANSWER":false,"answer_id":"one"}`} {
		call(http.MethodPost, exact+"/answer", responder, body, http.StatusBadRequest)
	}
	body := `{"answer":"","answer_id":"answer"}`
	receipt := call(http.MethodPost, exact+"/answer", responder, body, http.StatusOK)
	if receipt["question"] != nil || string(receipt["answer"]) != `""` || string(receipt["answered_by_api_key_id"]) != `"`+responderID.String()+`"` {
		t.Fatalf("answer receipt %v", receipt)
	}
	observed := operation(workerapi.AgentMethodWaitAsk, map[string]any{"askId": ids[0]})
	var resolved struct {
		Answer     json.RawMessage           `json:"answer"`
		AnsweredBy struct{ Kind, ID string } `json:"answeredBy"`
	}
	if err := json.Unmarshal(observed.Value, &resolved); err != nil || observed.Error != nil || string(resolved.Answer) != `""` || resolved.AnsweredBy.Kind != "api_key" || resolved.AnsweredBy.ID != responderID.String() {
		t.Fatalf("runtime answer %+v %v", observed, err)
	}
	if result := operation(workerapi.AgentMethodWithdrawAsk, map[string]any{"askId": ids[0]}); result.Error != nil {
		t.Fatal(result.Error)
	}
	// A human operator can answer with explicit run permissions.
	f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.answer")
	login := hf.session(t, f.User, org)
	humanPath := fmt.Sprintf("/api/projects/project/environments/test/sessions/%s/turns/%s/asks/%s", f.Session, admission.TurnID, ids[1])
	call(http.MethodGet, humanPath, login, "", http.StatusOK)
	human := call(http.MethodPost, humanPath+"/answer", login, `{"answer":"Allow","answer_id":"human"}`, http.StatusOK)
	if string(human["answered_by_user_id"]) != `"`+f.User.String()+`"` {
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
	if string(expired["payload_expired"]) != "true" || expired["answer"] != nil || expired["question"] != nil || expired["answered_by_api_key_id"] == nil {
		t.Fatalf("expired payload %v", expired)
	}
}

func TestWorkerAgentAskObservationsReleaseCapacity(t *testing.T) {
	f := sessiontest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	sessionID := runtimeTestSession(f)
	a, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "many asks", Input: json.RawMessage(`[]`)})
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ids := make([]uuid.UUID, 65)
	req := workerapi.AgentOperationRequest{Session: sessionID, AuthorityGeneration: attachment.AuthorityGeneration, TurnID: a.TurnID.String()}
	for i := range ids {
		ids[i] = uuid.NewV7()
		req.RequestID = fmt.Sprintf("create-%d", i)
		req.Method = workerapi.AgentMethodAsk
		req.Payload, _ = json.Marshal(map[string]any{"creationId": ids[i], "question": json.RawMessage(`{"prompt":[],"type":"text"}`)})
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
			r.Method = workerapi.AgentMethodWaitAsk
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
	req.Method = workerapi.AgentMethodWithdrawAsk
	req.Payload, _ = json.Marshal(map[string]any{"askId": ids[0]})
	if result, err := client.AgentOperation(ctx, req); err != nil || result.Error != nil {
		t.Fatalf("withdraw %+v %v", result, err)
	}
	req.RequestID = "observe-cancelled"
	req.Method = workerapi.AgentMethodWaitAsk
	if result, err := client.AgentOperation(ctx, req); err != nil || result.Error == nil || result.Error.Code != "ask_cancelled" {
		t.Fatalf("cancel observation %+v %v", result, err)
	}
}
