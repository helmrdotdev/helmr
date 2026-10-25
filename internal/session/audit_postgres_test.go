package session_test

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"io"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestAuditStartAtomicReceiptAndCredential(t *testing.T) {
	f := newAdmissionFixture(t)
	caller := f.caller()
	caller.CredentialID, caller.CredentialKind = uuid.NewV7(), "login_session"
	req := f.startRequest("audit-start")
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	receipt, err := session.Start(t.Context(), policy, f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Start(t.Context(), policy, f.pool, nil, caller, req); err != nil {
		t.Fatal(err)
	}
	var count, operations int
	var valid bool
	err = f.pool.QueryRow(t.Context(), `SELECT count(*),count(DISTINCT operation_id),bool_and(actor_user_id IS NOT NULL AND actor_user_id=$2 AND credential_id=$3 AND environment_id=$4 AND authorization_evidence->'checks' @> '[{"type":"permission","result":"passed"}]'::jsonb AND actor_identity->>'source'='account') FROM audit_events WHERE session_id=$1`, receipt.SessionID, caller.ID, caller.CredentialID, f.env).Scan(&count, &operations, &valid)
	if err != nil || count != 4 || operations != 1 || !valid {
		t.Fatalf("capture/replay: count=%d operations=%d valid=%v err=%v", count, operations, valid, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_session_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_session_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_session_audit()`)
	req.IdempotencyKey = "audit-failed"
	if _, err = session.Start(t.Context(), policy, f.pool, nil, caller, req); !errors.Is(err, audit.ErrCapture) {
		t.Fatalf("capture failure: %v", err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE environment_id=$1 AND creation_idempotency_key='audit-failed'`, f.env).Scan(&count); err != nil || count != 0 {
		t.Fatalf("creation escaped rollback: %d %v", count, err)
	}
}

func TestAuditContentCaptureFailureWithholdsData(t *testing.T) {
	f := newAdmissionFixture(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	receipt, err := session.Start(t.Context(), policy, f.pool, nil, f.caller(), f.startRequest("content-audit"))
	if err != nil {
		t.Fatal(err)
	}
	view, err := session.GetTurn(t.Context(), policy, f.pool, f.caller(), f.env, receipt.SessionID, receipt.TurnID)
	if err != nil || len(view.Input) == 0 {
		t.Fatalf("initial read: %v", err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.content_read'`, receipt.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("access record: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_content_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_content_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_content_audit()`)
	view, err = session.GetTurn(t.Context(), policy, f.pool, f.caller(), f.env, receipt.SessionID, receipt.TurnID)
	if !errors.Is(err, audit.ErrCapture) || len(view.Input) > 0 {
		t.Fatalf("unaudited disclosure: input=%d err=%v", len(view.Input), err)
	}
}

func TestAuditControlRollsBackAndDeduplicates(t *testing.T) {
	f := newAdmissionFixture(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	req := f.startRequest("control-audit")
	req.Input = nil
	receipt, err := session.Start(t.Context(), policy, f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_control_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_control_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_control_audit()`)
	control := session.SessionControlRequest{EnvironmentID: f.env, SessionID: receipt.SessionID, Kind: "close", IdempotencyKey: "close"}
	if _, err = session.ControlSession(t.Context(), policy, f.pool, f.caller(), control); !errors.Is(err, audit.ErrCapture) {
		t.Fatalf("capture failure: %v", err)
	}
	var status string
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, receipt.SessionID).Scan(&status); err != nil || status != "open" {
		t.Fatalf("close escaped rollback: %s %v", status, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_control_audit ON audit_events`)
	for range 2 {
		if _, err = session.ControlSession(t.Context(), policy, f.pool, f.caller(), control); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.control_requested' AND details->>'kind'='close'`, receipt.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("control receipt audit: %d %v", count, err)
	}
}

func TestAuditDeadlineSettlementRollsBackAndDoesNotRecreate(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "audit-deadline")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeCloseProcessing(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, a.TurnID)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_deadline_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_deadline_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_deadline_audit()`)
	fail := func() error {
		_, err := session.RuntimeFail(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), f.execution(), a.TurnID, session.TurnError{Code: "error"}, "drained")
		return err
	}
	if err := fail(); !errors.Is(err, audit.ErrCapture) {
		t.Fatalf("capture failure: %v", err)
	}
	var status string
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, a.TurnID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("deadline escaped rollback: %s %v", status, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_deadline_audit ON audit_events`)
	if err := fail(); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE session_id=$1 AND actor_component='control_plane' AND action IN ('turn.interrupted','session.authority_changed')`, f.session).Scan(&count); err != nil || count != 2 {
		t.Fatalf("deadline facts: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM audit_events WHERE session_id=$1`, f.session)
	if err := fail(); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE session_id=$1`, f.session).Scan(&count); err != nil || count != 0 {
		t.Fatalf("recreated facts: %d %v", count, err)
	}
}

func TestAuditWorkerStopAttributionAndRollback(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "audit-worker-stop")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_stop_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_stop_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_stop_audit()`)
	stop := func() error {
		return session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), f.execution(), attachment.Sequence)
	}
	if err := stop(); !errors.Is(err, audit.ErrCapture) {
		t.Fatalf("capture failure: %v", err)
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, a.TurnID).Scan(&state); err != nil || state != "running" {
		t.Fatalf("stop escaped rollback: %s %v", state, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_stop_audit ON audit_events`)
	for range 2 {
		if err := stop(); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var valid bool
	err = f.pool.QueryRow(t.Context(), `SELECT count(*),bool_and(actor_worker_host_id IS NOT NULL AND credential_id=$2 AND credential_version=$3) FROM audit_events WHERE session_id=$1 AND action IN ('turn.interrupted','session.authority_changed')`, f.session, f.host().HostID, f.host().HostClaimVersion).Scan(&count, &valid)
	if err != nil || count != 2 || !valid {
		t.Fatalf("worker facts: %d %v %v", count, valid, err)
	}
}

func TestAuditComputerControlSettlementRollback(t *testing.T) {
	f := newFixture(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	accepted, err := session.ControlComputerExecution(t.Context(), policy, f.pool, f.caller(), f.executionRequest("hibernate", "audit-settlement"))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_controls SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, accepted.ID)
	dbtest.MustExec(t, t.Context(), f.pool, `ALTER TABLE audit_events ADD CONSTRAINT reject_control_settlement CHECK(action<>'computer.control_settled') NOT VALID`)
	if err := computer.ReconcileExecutionControls(t.Context(), policy, f.pool); !errors.Is(err, audit.ErrCapture) {
		t.Fatalf("capture failure: %v", err)
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_controls WHERE id=$1`, accepted.ID).Scan(&status); err != nil || status != "accepted" {
		t.Fatalf("settlement escaped rollback: %s %v", status, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `ALTER TABLE audit_events DROP CONSTRAINT reject_control_settlement`)
	for range 2 {
		if err := computer.ReconcileExecutionControls(t.Context(), policy, f.pool); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE computer_id=$1 AND action='computer.control_settled' AND outcome='failed' AND details->>'request_id'=$2`, f.computer, accepted.ID.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("settlement facts: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM audit_events WHERE action='computer.control_settled'`)
	if err := computer.ReconcileExecutionControls(t.Context(), policy, f.pool); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='computer.control_settled'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("settlement recreated: %d %v", count, err)
	}
}

func TestAuditComputerControlRejectsRotatedCredentialAfterOwnerWait(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "replay"}[replay], func(t *testing.T) {
			f := newFixture(t)
			key := uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) SELECT $1,org_id,project_id,id,'rotation','rotation',$2 FROM environments WHERE id=$3`, key, []byte(key.String()), f.env)
			dbtest.SetAPIKeyRole(t, f.pool, key, "computers.hibernate")
			caller := session.Caller{Kind: "api_key", ID: key, CredentialVersion: 1}
			policy := auditpolicy.Policy{RetentionSeconds: 3600}
			req := f.executionRequest("hibernate", "rotation")
			if replay {
				if _, err := session.ControlComputerExecution(t.Context(), policy, f.pool, caller, req); err != nil {
					t.Fatal(err)
				}
			}
			hold, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(context.Background())
			if _, err = hold.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR UPDATE`, f.computer); err != nil {
				t.Fatal(err)
			}
			if _, err = hold.Exec(t.Context(), `UPDATE api_keys SET credential_version=2 WHERE id=$1`, key); err != nil {
				t.Fatal(err)
			}
			err = commitWithBlockedOperation(t, f, hold, func(ctx context.Context) error {
				_, err := session.ControlComputerExecution(ctx, policy, f.pool, caller, req)
				return err
			})
			if !errors.Is(err, session.ErrDenied) {
				t.Fatalf("rotated credential admitted: %v", err)
			}
			var n int
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='computer.control_requested' AND computer_id=$1`, f.computer).Scan(&n); err != nil {
				t.Fatal(err)
			}
			want := 0
			if replay {
				want = 1
			}
			if n != want {
				t.Fatalf("unexpected audit facts: %d", n)
			}
		})
	}
}

type lostAuditCommit struct{ db.TxBeginner }

func (b lostAuditCommit) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return lostAuditCommitTx{tx}, nil
}

type lostAuditCommitTx struct{ pgx.Tx }

func (tx lostAuditCommitTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	return io.EOF
}
func TestAuditMCPAmbiguousAdmissionDoesNotRecordRefusal(t *testing.T) {
	f := newFixture(t)
	caller := session.Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	err := session.AdmitMCP(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, lostAuditCommit{f.pool}, caller, "list_agents")
	if !db.CommitOutcomeUnknown(err) {
		t.Fatal(err)
	}
	var admitted, refused int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE action='mcp.admitted'),count(*) FILTER(WHERE action='mcp.refused') FROM audit_events`).Scan(&admitted, &refused); err != nil || admitted != 1 || refused != 0 {
		t.Fatal("ambiguous admission misclassified", admitted, refused, err)
	}
}

func TestAuditSessionHoldReadsWithholdOnCaptureFailure(t *testing.T) {
	f := newAdmissionFixture(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	receipt, err := session.Start(t.Context(), policy, f.pool, nil, f.caller(), f.startRequest("hold-disclosure"))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','confidential hold text')`, f.env, uuid.NewV7(), receipt.SessionID)
	view, err := session.GetSession(t.Context(), policy, f.pool, f.caller(), f.env, receipt.SessionID)
	if err != nil || len(view.Holds) != 1 {
		t.Fatalf("get: %+v %v", view, err)
	}
	page, err := session.ListSessions(t.Context(), policy, f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Limit: 100})
	if err != nil || len(page.Sessions) == 0 {
		t.Fatalf("list: %+v %v", page, err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.content_read'`, receipt.SessionID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("capture: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_hold_read_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_hold_read_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_hold_read_audit()`)
	view, err = session.GetSession(t.Context(), policy, f.pool, f.caller(), f.env, receipt.SessionID)
	if !errors.Is(err, audit.ErrCapture) || len(view.Holds) != 0 {
		t.Fatalf("get escaped: %+v %v", view, err)
	}
	page, err = session.ListSessions(t.Context(), policy, f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Limit: 100})
	if !errors.Is(err, audit.ErrCapture) || len(page.Sessions) != 0 {
		t.Fatalf("list escaped: %+v %v", page, err)
	}
	if err := session.AuthorizeSessionRead(t.Context(), f.pool, f.caller(), f.env, receipt.SessionID); err != nil {
		t.Fatalf("metadata subscription: %v", err)
	}
}

func TestAuditMCPStaleGenerationRetainsObservedCheck(t *testing.T) {
	f := newFixture(t)
	e := f.execution()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.session)
	caller := session.Caller{Kind: "session", ID: f.session, Execution: e, Host: f.host()}
	err := session.AdmitMCP(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, "list_agents")
	if !errors.Is(err, session.ErrDenied) {
		t.Fatal(err)
	}
	var valid bool
	err = f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(actor_worker_host_id=$1 AND actor_session_id IS NULL AND org_id IS NULL AND authentication_result='verified' AND authorization_evidence->>'decision'='denied' AND authorization_evidence->'checks'->0->>'type'='runtime_binding' AND authorization_evidence->'checks'->0->>'reason'='generation_changed' AND (authorization_evidence->'checks'->0->>'generation')::bigint=$2 AND (authorization_evidence->'checks'->0->>'observed_generation')::bigint=$2+1) FROM audit_events WHERE action='mcp.refused'`, f.host().HostID, e.AuthorityGeneration).Scan(&valid)
	if err != nil || !valid {
		t.Fatal("lost runtime refusal or changed executor", valid, err)
	}
}
