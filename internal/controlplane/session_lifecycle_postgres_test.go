package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestSessionHTTPPostgresAdmissionEventsScopeAndClose(t *testing.T) {
	f := newSessionHTTP(t, sessiontest.New(t, 1))
	started := startSession(t, f.Fixture, 0, nil, "")
	principal := auth.Principal{OrgID: f.OrgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper, ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String()}
	token := f.apiKey(principal)
	call := func(method, route, raw string) *httptest.ResponseRecorder {
		t.Helper()
		return f.request(t, method, "/v1/sessions/"+started.SessionID.String()+route, token, raw)
	}
	assert := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
	}
	raw := `{"data":null,"idempotency_key":"first"}`
	assert(call(http.MethodPost, "/send", raw), http.StatusForbidden)
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_turns WHERE session_id=$1`, started.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied admission residue=%d err=%v", count, err)
	}
	principal.Permissions = []auth.Permission{auth.PermissionSessionsSend, auth.PermissionSessionsRead, auth.PermissionSessionsClose, auth.PermissionSessionsInterrupt}
	token = f.apiKey(principal)
	w := call(http.MethodPost, "/send", raw)
	assert(w, http.StatusAccepted)
	var first api.SessionAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "enqueued" || first.ID == "" || first.TurnID == "" || first.MessageID != nil {
		t.Fatalf("first=%+v", first)
	}
	replay := call(http.MethodPost, "/send", raw)
	assert(replay, http.StatusAccepted)
	if replay.Body.String() != w.Body.String() {
		t.Fatalf("replay retargeted: %s vs %s", replay.Body.String(), w.Body.String())
	}
	w = call(http.MethodPost, "/send", `{"data":1,"idempotency_key":"first"}`)
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "idempotency_conflict" {
		t.Fatalf("conflict=%s", w.Body.String())
	}
	w = call(http.MethodPost, "/enqueue", `{"data":{"type":"next"},"idempotency_key":"second"}`)
	assert(w, http.StatusAccepted)
	var second api.SessionAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.TurnID == first.TurnID {
		t.Fatal("two admissions share one Turn")
	}
	w = call(http.MethodPost, "/turns/"+uuid.NewV7().String()+"/interrupt", `{"idempotency_key":"missing-turn"}`)
	assert(w, http.StatusNotFound)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "turn_not_found" {
		t.Fatalf("missing Turn=%s", w.Body.String())
	}
	w = call(http.MethodGet, "/turns/"+first.TurnID, "")
	assert(w, http.StatusOK)
	var turn api.SessionTurn
	if err := json.Unmarshal(w.Body.Bytes(), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Status != "queued" || turn.Sequence != 1 || string(turn.Input) != "null" || turn.AcceptsMessages {
		t.Fatalf("turn=%+v", turn)
	}
	w = call(http.MethodGet, "/events?after=0&limit=1", "")
	assert(w, http.StatusOK)
	var page api.SessionEventPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.NextAfter != 1 || !page.HasMore || page.RetainedAfter != 0 || page.Records[0].Kind != "turn.enqueued" || page.Records[0].TurnID == nil || *page.Records[0].TurnID != first.TurnID || page.Records[0].Provenance != nil {
		t.Fatalf("page=%+v", page)
	}
	w = call(http.MethodGet, "/events?after=2", "")
	assert(w, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Records == nil || len(page.Records) != 0 || page.NextAfter != 2 || page.HasMore {
		t.Fatalf("empty=%+v", page)
	}
	w = call(http.MethodGet, "/events?after=999", "")
	assert(w, http.StatusBadRequest)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "invalid_cursor" {
		t.Fatalf("future=%s", w.Body.String())
	}
	allowed := token
	principal.EnvironmentID = uuid.NewV7().String()
	token = f.apiKey(principal)
	assert(call(http.MethodGet, "/events", ""), http.StatusNotFound)
	token = allowed
	w = call(http.MethodPost, "/close", `{"idempotency_key":"close"}`)
	assert(w, http.StatusAccepted)
	w = call(http.MethodGet, "", "")
	assert(w, http.StatusOK)
	var snapshot api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != api.SessionStatusClosing || snapshot.Dispatch.State != "ready" || snapshot.ActiveTurnID != nil {
		t.Fatalf("closing=%+v", snapshot)
	}
	w = call(http.MethodPost, "/send", `{"data":3,"idempotency_key":"after-close"}`)
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "session_not_open" {
		t.Fatalf("closed admission=%s", w.Body.String())
	}
	// An admitted operation retains its exact receipt even after close.
	w = call(http.MethodPost, "/send", raw)
	assert(w, http.StatusAccepted)
	var afterClose api.SessionAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &afterClose); err != nil {
		t.Fatal(err)
	}
	if afterClose.ID != first.ID || afterClose.TurnID != first.TurnID {
		t.Fatalf("after close=%+v", afterClose)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_turns WHERE session_id=$1`, started.SessionID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("turns=%d err=%v", count, err)
	}
}

func TestSessionHTTPPostgresStopKeepsFIFOAndRequiresExactHold(t *testing.T) {
	f := newActorExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	token := f.apiKey(auth.Principal{OrgID: f.OrgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper, ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String(), Permissions: []auth.Permission{auth.PermissionSessionsSend, auth.PermissionSessionsInterrupt, auth.PermissionSessionsResume, auth.PermissionSessionsRead, auth.PermissionSessionsClose}})
	call := func(method, route, raw string) *httptest.ResponseRecorder {
		t.Helper()
		return f.request(t, method, "/v1/sessions/"+f.SessionID.String()+route, token, raw)
	}
	assert := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
	}
	assert(call(http.MethodPost, "/enqueue", `{"data":"B","idempotency_key":"B"}`), http.StatusAccepted)
	w := call(http.MethodPost, "/turns/"+scope.TurnID.String()+"/interrupt", `{"idempotency_key":"interrupt-A"}`)
	assert(w, http.StatusAccepted)
	var receipt api.TurnInterruptReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.TurnID != scope.TurnID.String() || receipt.HoldID == "" || receipt.Status != "accepted" {
		t.Fatalf("interrupt=%+v", receipt)
	}
	assert(call(http.MethodPost, "/enqueue", `{"data":"C","idempotency_key":"C"}`), http.StatusAccepted)
	w = call(http.MethodPost, "/send", `{"data":"D","idempotency_key":"D"}`)
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "session_held" {
		t.Fatalf("held admission=%s", w.Body.String())
	}
	w = call(http.MethodPost, "/resume", `{"hold_id":"`+uuid.NewV7().String()+`","idempotency_key":"stale-resume"}`)
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "stale_hold" {
		t.Fatalf("resume=%s", w.Body.String())
	}
	w = call(http.MethodPost, "/resume", `{"hold_id":"`+receipt.HoldID+`","idempotency_key":"early-resume"}`)
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "not_settled" {
		t.Fatalf("resume before convergence=%s", w.Body.String())
	}
	assert(call(http.MethodPost, "/close", `{}`), http.StatusAccepted)
	w = call(http.MethodGet, "/turns/"+scope.TurnID.String(), "")
	assert(w, http.StatusOK)
	var turn api.SessionTurn
	if err := json.Unmarshal(w.Body.Bytes(), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Status != "running" || !turn.InterruptRequested || turn.AcceptsMessages || turn.TerminalEventID != nil {
		t.Fatalf("acceptance claimed terminal: %+v", turn)
	}
	w = call(http.MethodGet, "", "")
	assert(w, http.StatusOK)
	var snapshot api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != api.SessionStatusClosing || snapshot.Dispatch.HoldID == nil || *snapshot.Dispatch.HoldID != receipt.HoldID {
		t.Fatalf("close cleared hold: %+v", snapshot)
	}
	var sequences []int64
	rows, err := f.Pool.Query(t.Context(), `SELECT sequence FROM session_turns WHERE session_id=$1 AND status='queued' ORDER BY sequence`, f.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		sequences = append(sequences, n)
	}
	if rows.Err() != nil || len(sequences) != 2 || sequences[0] != 2 || sequences[1] != 3 {
		t.Fatalf("FIFO=%v err=%v", sequences, rows.Err())
	}
}

func TestSessionHTTPPostgresIdleCloseReleasesComputer(t *testing.T) {
	f := newSessionHTTP(t, sessiontest.New(t, 1))
	started := startSession(t, f.Fixture, 0, nil, "")
	settleActorBootRun(t, f.Fixture, started, 0)
	token := f.memberSession(t, db.OrgMemberRoleDeveloper)
	w := f.request(t, http.MethodPost, f.environmentPath("/sessions/"+started.SessionID.String()+"/close"), token, `{"idempotency_key":"close-idle"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	reconciler, err := session.NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if deferred, err := reconciler.ReconcileLifecycle(t.Context(), f.EnvironmentID, started.SessionID); err != nil || deferred {
		t.Fatalf("close reconciliation deferred=%v err=%v", deferred, err)
	}
	var status string
	var sessionComputer *uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.status,s.computer_id FROM sessions s JOIN computers w ON w.id=s.computer_id WHERE s.id=$1`, started.SessionID).Scan(&status, &sessionComputer); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || sessionComputer == nil {
		t.Fatalf("close=%s sessionComputer=%v", status, sessionComputer)
	}
	// The console route reads the closed Session through the same scope.
	w = f.request(t, http.MethodGet, f.environmentPath("/sessions?status=closed"), token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list=%d %s", w.Code, w.Body.String())
	}
	var listed api.ListSessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != started.SessionID.String() {
		t.Fatalf("closed Sessions = %+v", listed.Sessions)
	}
}
