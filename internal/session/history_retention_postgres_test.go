package session_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func reconcileHistory(t *testing.T, f fixture) {
	t.Helper()
	if err := slack.RetainSessionHistory(t.Context(), f.pool, f.env, f.session); err != nil {
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
	req := session.AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: id, ResponseID: "answer", Answer: json.RawMessage(`""`)}
	if _, err := session.RespondAsk(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), json.RawMessage(`[{"type":"text","text":"response"}]`)); err != nil {
		t.Fatal(err)
	}
	reconcileHistory(t, f)
	if historyClock(t, f) != nil {
		t.Fatal("open Session started clock")
	}
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "stop"}); err != nil {
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
	view, err := session.GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID)
	if err != nil || view.Input == nil || view.PayloadExpiredAt != nil {
		t.Fatalf("premature expiry: %+v %v", view, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_eligible_at=statement_timestamp()-interval '2 minutes',history_expires_at=EXTRACT(epoch FROM statement_timestamp()-interval '2 minutes')+history_retention_seconds WHERE id=$1`, f.session)
	reconcileHistory(t, f)
	reconcileHistory(t, f)
	view, err = session.GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID)
	if err != nil || view.Input != nil || view.Result != nil || view.Response != nil || view.PayloadExpiredAt == nil || view.TerminalAt == nil || view.Status != "cancelled" {
		t.Fatalf("expired Turn: %+v %v", view, err)
	}
	answer, err := session.GetAsk(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID, id)
	if err != nil || answer.Question != nil || answer.Answer != nil || answer.PayloadExpiredAt == nil || answer.RespondedByUserID == nil {
		t.Fatalf("expired answer: %+v %v", answer, err)
	}
	replay, err := session.RespondAsk(t.Context(), f.pool, f.caller(), req)
	if err != nil || replay.PayloadExpiredAt == nil {
		t.Fatalf("answer replay: %+v %v", replay, err)
	}
	if retry := f.enqueue(t, "ask"); retry.TurnID != a.TurnID {
		t.Fatal("admission retry changed")
	}
	_, err = session.ListEvents(t.Context(), f.pool, f.caller(), session.TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 100})
	var expired *session.CursorExpired
	if !errors.As(err, &expired) || expired.RetainedAfter == 0 {
		t.Fatalf("cursor floor: %v", err)
	}
	page, err := session.ListEvents(t.Context(), f.pool, f.caller(), session.TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 100, After: expired.RetainedAfter})
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
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "stop"}); err != nil {
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
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE checkpoints SET status='lost',terminal_evidence='continuation unavailable',capture_request=NULL WHERE environment_id=$1`, f.env)
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
	next, more, err := slack.ReconcileHistory(t.Context(), f.pool, slack.HistoryPosition{})
	if err != nil || !more {
		t.Fatalf("first page: %v %v", more, err)
	}
	if historyClock(t, ready) != nil {
		t.Fatal("last owner should be on second page")
	}
	_, more, err = slack.ReconcileHistory(t.Context(), f.pool, next)
	if err != nil || more || historyClock(t, ready) == nil {
		t.Fatalf("blocked prefix starved later owner: %v %v", more, err)
	}
}

func TestHistoryRetentionMaximumDurationIsExact(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "max-duration")
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "stop"}); err != nil {
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

func TestHistoryRetentionRollsBackWhenSlackPurgeFails(t *testing.T) {
	f := newFixture(t)
	turn := f.enqueue(t, "retention-rollback")
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	fenceHistoryProcess(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET history_retention_mode='duration',history_retention_seconds=1,history_eligible_at=statement_timestamp()-interval '2 seconds',history_expires_at=EXTRACT(epoch FROM statement_timestamp())-1 WHERE id=$1`, f.session)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_slack_history_purge() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected Slack history purge failure'; END $$;
 CREATE TRIGGER reject_slack_history_purge BEFORE UPDATE ON slack_requests FOR EACH STATEMENT EXECUTE FUNCTION reject_slack_history_purge()`)
	if err := slack.RetainSessionHistory(t.Context(), f.pool, f.env, f.session); err == nil {
		t.Fatal("Slack purge failure committed Session expiry")
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT s.history_expired_at IS NULL AND t.input IS NOT NULL AND t.payload_expired_at IS NULL FROM sessions s JOIN turns t ON t.environment_id=s.environment_id AND t.session_id=s.id WHERE t.id=$1`, turn.TurnID).Scan(&retained); err != nil || !retained {
		t.Fatalf("Session content did not roll back: %v %v", retained, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_slack_history_purge ON slack_requests; DROP FUNCTION reject_slack_history_purge()`)
	reconcileHistory(t, f)
	var expired bool
	if err := f.pool.QueryRow(t.Context(), `SELECT s.history_expired_at IS NOT NULL AND t.input IS NULL AND t.payload_expired_at IS NOT NULL FROM sessions s JOIN turns t ON t.environment_id=s.environment_id AND t.session_id=s.id WHERE t.id=$1`, turn.TurnID).Scan(&expired); err != nil || !expired {
		t.Fatalf("retry did not expire Session content: %v %v", expired, err)
	}
}
