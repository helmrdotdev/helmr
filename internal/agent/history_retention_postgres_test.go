package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func reconcileHistory(t *testing.T, f fixture) {
	t.Helper()
	if err := retainSessionHistory(t.Context(), f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
}
func historyClock(t *testing.T, f fixture) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT history_eligible_at FROM sessions WHERE id=$1`, f.session).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}
func fenceHistoryProcess(t *testing.T, f fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.session)
}
func TestHistoryRetentionClockPurgeAndReplay(t *testing.T) {
	f, a, id, _ := askFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_retention_mode='duration',history_retention_seconds=60 WHERE id=$1`, f.session)
	req := AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: id, ResponseID: "answer", Answer: json.RawMessage(`""`)}
	if _, err := RespondAsk(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), json.RawMessage(`[{"type":"text","text":"response"}]`)); err != nil {
		t.Fatal(err)
	}
	reconcileHistory(t, f)
	if historyClock(t, f) != nil {
		t.Fatal("open Session started clock")
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", RetryKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='lost' WHERE session_id=$1`, f.session)
	reconcileHistory(t, f)
	if historyClock(t, f) != nil {
		t.Fatal("uncertain physical stop started clock")
	}
	fenceHistoryProcess(t, f)
	reconcileHistory(t, f)
	first := historyClock(t, f)
	if first == nil {
		t.Fatal("released owner has no clock")
	}
	reconcileHistory(t, f)
	if got := historyClock(t, f); got == nil || !got.Equal(*first) {
		t.Fatal("clock moved")
	}
	view, err := GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID)
	if err != nil || view.Input == nil || view.PayloadExpiredAt != nil {
		t.Fatalf("premature expiry: %+v %v", view, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_eligible_at=statement_timestamp()-interval '2 minutes',history_expires_at=EXTRACT(epoch FROM statement_timestamp()-interval '2 minutes')+history_retention_seconds WHERE id=$1`, f.session)
	reconcileHistory(t, f)
	reconcileHistory(t, f)
	view, err = GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID)
	if err != nil || view.Input != nil || view.Result != nil || view.Response != nil || view.PayloadExpiredAt == nil || view.TerminalAt == nil || view.Status != "cancelled" {
		t.Fatalf("expired Turn: %+v %v", view, err)
	}
	answer, err := GetAsk(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID, id)
	if err != nil || answer.Question != nil || answer.Answer != nil || answer.PayloadExpiredAt == nil || answer.RespondedByUserID == nil {
		t.Fatalf("expired answer: %+v %v", answer, err)
	}
	replay, err := RespondAsk(t.Context(), f.pool, f.caller(), req)
	if err != nil || replay.PayloadExpiredAt == nil {
		t.Fatalf("answer replay: %+v %v", replay, err)
	}
	if retry := f.enqueue(t, "ask"); retry.TurnID != a.TurnID {
		t.Fatal("admission retry changed")
	}
	_, err = ListEvents(t.Context(), f.pool, f.caller(), TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 100})
	var expired *CursorExpired
	if !errors.As(err, &expired) || expired.RetainedAfter == 0 {
		t.Fatalf("cursor floor: %v", err)
	}
	page, err := ListEvents(t.Context(), f.pool, f.caller(), TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 100, After: expired.RetainedAfter})
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("expired event read: %+v %v", page, err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT input IS NULL AND response IS NULL AND response_id IS NOT NULL AND response_digest IS NOT NULL AND request_digest IS NOT NULL AND response_expired_at IS NOT NULL FROM turns WHERE id=$1`, a.TurnID).Scan(&retained); err != nil || !retained {
		t.Fatalf("lost identities: %v %v", retained, err)
	}
}
func TestHistoryRetentionPendingSaveAndUntilDeletion(t *testing.T) {
	f := newFixture(t)
	_, save := f.finalize(t, "save")
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", RetryKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	fenceHistoryProcess(t, f)
	reconcileHistory(t, f)
	if historyClock(t, f) != nil {
		t.Fatal("pending save started clock")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='confirmed no publication' WHERE id=$1`, save.ID)
	reconcileHistory(t, f)
	if historyClock(t, f) == nil {
		t.Fatal("released until-deletion owner has no eligibility")
	}
	var kept bool
	if err := f.pool.QueryRow(t.Context(), `SELECT history_expires_at IS NULL AND history_expired_at IS NULL AND EXISTS(SELECT 1 FROM turns WHERE session_id=$1 AND input IS NOT NULL) FROM sessions WHERE id=$1`, f.session).Scan(&kept); err != nil || !kept {
		t.Fatalf("until-deletion content lost: %v %v", kept, err)
	}
}
func TestHistoryRetentionCheckpointObligation(t *testing.T) {
	c := readyCheckpointFixture(t)
	f := c.f
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled' WHERE id=$1`, f.session)
	fenceHistoryProcess(t, f)
	reconcileHistory(t, f)
	if historyClock(t, f) != nil {
		t.Fatal("continuation started history clock")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_checkpoints SET status='lost',terminal_evidence='continuation unavailable',capture_request=NULL WHERE environment_id=$1`, f.env)
	reconcileHistory(t, f)
	if historyClock(t, f) == nil {
		t.Fatal("released continuation did not start clock")
	}
}

func TestHistoryRetentionKeysetPassesObligatedPrefix(t *testing.T) {
	f := newFixture(t)
	for range 101 {
		p := f.peer(t)
		dbtest.MustExec(t, t.Context(), p.pool, `UPDATE sessions SET status='cancelled' WHERE id=$1`, p.session)
	}
	var last uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sessions WHERE status='cancelled' ORDER BY environment_id DESC,id DESC LIMIT 1`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	ready := f
	ready.session = last
	fenceHistoryProcess(t, ready)
	next, more, err := reconcileSessionHistory(t.Context(), f.pool, sessionLifecyclePosition{})
	if err != nil || !more {
		t.Fatalf("first page: %v %v", more, err)
	}
	if historyClock(t, ready) != nil {
		t.Fatal("last owner should be on second page")
	}
	_, more, err = reconcileSessionHistory(t.Context(), f.pool, next)
	if err != nil || more || historyClock(t, ready) == nil {
		t.Fatalf("blocked prefix starved later owner: %v %v", more, err)
	}
}

func TestHistoryRetentionMaximumDurationIsExact(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "max-duration")
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", RetryKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	fenceHistoryProcess(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_retention_mode='duration',history_retention_seconds=9223372036854775807 WHERE id=$1`, f.session)
	reconcileHistory(t, f)
	var exact bool
	if err := f.pool.QueryRow(t.Context(), `SELECT history_expires_at-EXTRACT(epoch FROM history_eligible_at)=9223372036854775807 AND history_expired_at IS NULL FROM sessions WHERE id=$1`, f.session).Scan(&exact); err != nil || !exact {
		t.Fatalf("maximum duration changed: %v %v", exact, err)
	}
	reconcileHistory(t, f)
	if historyClock(t, f) == nil {
		t.Fatal("maximum duration clock not recorded")
	}
}
