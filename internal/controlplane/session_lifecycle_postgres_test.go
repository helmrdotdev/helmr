package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestSessionHTTPPostgresAdmissionEventsScopeAndClose(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	started, err := f.server.startActor(t.Context(), f.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	principal := auth.Actor{OrgID: f.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper, ProjectID: f.projectID.String(), EnvironmentID: f.environmentID.String()}
	call := func(handler http.HandlerFunc, raw, turnID, query string) *httptest.ResponseRecorder {
		t.Helper()
		r := sessionLifecycleRequest(raw, principal, started.SessionID.String(), turnID)
		r.URL.RawQuery = query
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	assert := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
	}
	raw := `{"data":null,"idempotency_key":"first"}`
	assert(call(f.server.sendSessionHTTP, raw, "", ""), http.StatusForbidden)
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_turns WHERE session_id=$1`, started.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied admission residue=%d err=%v", count, err)
	}
	principal.Permissions = []auth.Permission{auth.PermissionSessionsSend, auth.PermissionSessionsRead, auth.PermissionSessionsClose, auth.PermissionSessionsInterrupt}
	w := call(f.server.sendSessionHTTP, raw, "", "")
	assert(w, http.StatusAccepted)
	var first api.SessionAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "enqueued" || first.ID == "" || first.TurnID == "" || first.MessageID != nil {
		t.Fatalf("first=%+v", first)
	}
	replay := call(f.server.sendSessionHTTP, raw, "", "")
	assert(replay, http.StatusAccepted)
	if replay.Body.String() != w.Body.String() {
		t.Fatalf("replay retargeted: %s vs %s", replay.Body.String(), w.Body.String())
	}
	w = call(f.server.sendSessionHTTP, `{"data":1,"idempotency_key":"first"}`, "", "")
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "idempotency_conflict" {
		t.Fatalf("conflict=%s", w.Body.String())
	}
	w = call(f.server.enqueueSessionHTTP, `{"data":{"type":"next"},"idempotency_key":"second"}`, "", "")
	assert(w, http.StatusAccepted)
	var second api.SessionAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.TurnID == first.TurnID {
		t.Fatal("two admissions share one Turn")
	}
	w = call(f.server.interruptSessionTurnHTTP, `{"idempotency_key":"missing-turn"}`, uuid.NewV7().String(), "")
	assert(w, http.StatusNotFound)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "turn_not_found" {
		t.Fatalf("missing Turn=%s", w.Body.String())
	}
	w = call(f.server.getSessionTurnHTTP, "", first.TurnID, "")
	assert(w, http.StatusOK)
	var turn api.SessionTurn
	if err := json.Unmarshal(w.Body.Bytes(), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Status != "queued" || turn.Sequence != 1 || string(turn.Input) != "null" || turn.AcceptsMessages {
		t.Fatalf("turn=%+v", turn)
	}
	w = call(f.server.readSessionEventsHTTP, "", "", "after=0&limit=1")
	assert(w, http.StatusOK)
	var page api.SessionEventPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.NextAfter != 1 || !page.HasMore || page.RetainedAfter != 0 || page.Records[0].Kind != "turn.enqueued" || page.Records[0].TurnID == nil || *page.Records[0].TurnID != first.TurnID || page.Records[0].Provenance != nil {
		t.Fatalf("page=%+v", page)
	}
	w = call(f.server.readSessionEventsHTTP, "", "", "after=2")
	assert(w, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Records == nil || len(page.Records) != 0 || page.NextAfter != 2 || page.HasMore {
		t.Fatalf("empty=%+v", page)
	}
	w = call(f.server.readSessionEventsHTTP, "", "", "after=999")
	assert(w, http.StatusBadRequest)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "invalid_cursor" {
		t.Fatalf("future=%s", w.Body.String())
	}
	principal.EnvironmentID = uuid.NewV7().String()
	assert(call(f.server.readSessionEventsHTTP, "", "", ""), http.StatusNotFound)
	principal.EnvironmentID = f.environmentID.String()
	w = call(f.server.closeSessionHTTP, `{"idempotency_key":"close"}`, "", "")
	assert(w, http.StatusAccepted)
	w = call(f.server.getSessionHTTP, "", "", "")
	assert(w, http.StatusOK)
	var snapshot api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != api.SessionStatusClosing || snapshot.Dispatch.State != "ready" || snapshot.ActiveTurnID != nil {
		t.Fatalf("closing=%+v", snapshot)
	}
	w = call(f.server.sendSessionHTTP, `{"data":3,"idempotency_key":"after-close"}`, "", "")
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "session_not_open" {
		t.Fatalf("closed admission=%s", w.Body.String())
	}
	// An admitted operation retains its exact receipt even after close.
	w = call(f.server.sendSessionHTTP, raw, "", "")
	assert(w, http.StatusAccepted)
	var afterClose api.SessionAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &afterClose); err != nil {
		t.Fatal(err)
	}
	if afterClose.ID != first.ID || afterClose.TurnID != first.TurnID {
		t.Fatalf("after close=%+v", afterClose)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_turns WHERE session_id=$1`, started.SessionID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("turns=%d err=%v", count, err)
	}
}

func TestSessionHTTPPostgresStopKeepsFIFOAndRequiresExactHold(t *testing.T) {
	f := newActorCheckpointFixture(t)
	scope := f.receiveTurn(t, 1)
	principal := auth.Actor{OrgID: f.OrgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper, ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String(), Permissions: []auth.Permission{auth.PermissionSessionsSend, auth.PermissionSessionsInterrupt, auth.PermissionSessionsResume, auth.PermissionSessionsRead, auth.PermissionSessionsClose}}
	call := func(handler http.HandlerFunc, raw, turnID string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler(w, sessionLifecycleRequest(raw, principal, f.sessionID.String(), turnID))
		return w
	}
	assert := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
	}
	assert(call(f.server.enqueueSessionHTTP, `{"data":"B","idempotency_key":"B"}`, ""), http.StatusAccepted)
	w := call(f.server.interruptSessionTurnHTTP, `{"idempotency_key":"interrupt-A"}`, scope.TurnID.String())
	assert(w, http.StatusAccepted)
	var receipt api.TurnInterruptReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.TurnID != scope.TurnID.String() || receipt.HoldID == "" || receipt.Status != "accepted" {
		t.Fatalf("interrupt=%+v", receipt)
	}
	assert(call(f.server.enqueueSessionHTTP, `{"data":"C","idempotency_key":"C"}`, ""), http.StatusAccepted)
	w = call(f.server.sendSessionHTTP, `{"data":"D","idempotency_key":"D"}`, "")
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "session_held" {
		t.Fatalf("held admission=%s", w.Body.String())
	}
	w = call(f.server.resumeSessionHTTP, `{"hold_id":"`+uuid.NewV7().String()+`","idempotency_key":"stale-resume"}`, "")
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "stale_hold" {
		t.Fatalf("resume=%s", w.Body.String())
	}
	w = call(f.server.resumeSessionHTTP, `{"hold_id":"`+receipt.HoldID+`","idempotency_key":"early-resume"}`, "")
	assert(w, http.StatusConflict)
	if decodeHTTPError(t, w.Body.Bytes()).Code != "not_settled" {
		t.Fatalf("resume before convergence=%s", w.Body.String())
	}
	assert(call(f.server.closeSessionHTTP, `{}`, ""), http.StatusAccepted)
	w = call(f.server.getSessionTurnHTTP, "", scope.TurnID.String())
	assert(w, http.StatusOK)
	var turn api.SessionTurn
	if err := json.Unmarshal(w.Body.Bytes(), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Status != "running" || !turn.InterruptRequested || turn.AcceptsMessages || turn.TerminalEventID != nil {
		t.Fatalf("acceptance claimed terminal: %+v", turn)
	}
	w = call(f.server.getSessionHTTP, "", "")
	assert(w, http.StatusOK)
	var snapshot api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != api.SessionStatusClosing || snapshot.Dispatch.HoldID == nil || *snapshot.Dispatch.HoldID != receipt.HoldID {
		t.Fatalf("close cleared hold: %+v", snapshot)
	}
	var sequences []int64
	rows, err := f.Pool.Query(t.Context(), `SELECT sequence FROM session_turns WHERE session_id=$1 AND status='queued' ORDER BY sequence`, f.sessionID)
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

func TestSessionHTTPPostgresIdleCloseReleasesWorkspace(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	started, err := f.server.startActor(t.Context(), f.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, f, started, 0)
	principal := auth.Actor{OrgID: f.orgID, Kind: auth.ActorKindSession, Role: auth.RoleDeveloper, ProjectID: f.projectID.String(), EnvironmentID: f.environmentID.String()}
	r := sessionLifecycleRequest(`{"idempotency_key":"close-idle"}`, principal, started.SessionID.String(), "")
	w := httptest.NewRecorder()
	f.server.closeSessionHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	reconciler, err := session.NewReconciler(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if deferred, err := reconciler.ReconcileLifecycle(t.Context(), f.environmentID, started.SessionID); err != nil || deferred {
		t.Fatalf("close reconciliation deferred=%v err=%v", deferred, err)
	}
	var status string
	var owner *uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT s.status,w.owner_session_id FROM sessions s JOIN computers w ON w.id=s.workspace_id WHERE s.id=$1`, started.SessionID).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || owner != nil {
		t.Fatalf("close=%s owner=%v", status, owner)
	}
	// The console route reads the closed Session through the same scope.
	r = sessionLifecycleRequest("", principal, started.SessionID.String(), "")
	r.URL = &url.URL{RawQuery: "status=closed"}
	w = httptest.NewRecorder()
	f.server.listSessionsHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list=%d %s", w.Code, w.Body.String())
	}
}
