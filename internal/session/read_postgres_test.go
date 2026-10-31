package session_test

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRetainedWorkReads(t *testing.T) {
	p := preparationResidentImage(t)
	f := p.f.fixture
	first := f.enqueue(t, "first")
	second, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "second", Input: json.RawMessage(`[{"type":"text","text":"{\"retained\":[1,\"two\",null]}"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.f.secrets.Revoke(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	page, err := session.ListTurns(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 1})
	if err != nil || len(page.Turns) != 1 || page.Turns[0].ID != first.TurnID || page.NextSequence != first.Sequence {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = session.ListTurns(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.TurnListRequest{EnvironmentID: f.env, SessionID: f.session, After: page.NextSequence, Limit: 1})
	if err != nil || len(page.Turns) != 1 || page.Turns[0].ID != second.TurnID || page.NextSequence != 0 {
		t.Fatalf("next page: %+v %v", page, err)
	}
	retained, err := session.GetTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session, second.TurnID)
	if err != nil || retained.Status != "queued" || len(retained.Result) != 0 {
		t.Fatalf("retained: %+v %v", retained, err)
	}
	want, _ := session.DigestJSON(json.RawMessage(`[{"type":"text","text":"{\"retained\":[1,\"two\",null]}"}]`))
	got, _ := session.DigestJSON(retained.Input)
	if got != want {
		t.Fatalf("input changed: %s", retained.Input)
	}
	if _, err := session.GetTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, uuid.NewV7(), second.TurnID); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("foreign Session: %v", err)
	}
	sessionID, err := session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session)
	if err != nil || sessionID.Status != "open" || sessionID.ComputerID == nil || *sessionID.ComputerID != f.computer {
		t.Fatalf("session: %+v %v", sessionID, err)
	}
	holdA, holdB := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,issuer_id) VALUES($1,$2,$4,'process_failed','system',NULL),($1,$3,$4,'interrupt','user',$5)`, f.env, holdA, holdB, f.session, f.user)
	sessionID, err = session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session)
	if err != nil || len(sessionID.Holds) != 2 {
		t.Fatalf("independent holds: %+v %v", sessionID, err)
	}
	for i, hold := range sessionID.Holds {
		if hold.ID != []uuid.UUID{holdA, holdB}[i] || hold.SessionID != f.session || hold.Kind != []string{"process_failed", "interrupt"}[i] || !hold.CanRelease || hold.CreatedAt.IsZero() {
			t.Fatalf("hold identity or timestamp lost: %+v", hold)
		}
	}
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	retained, err = session.GetTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session, second.TurnID)
	if err != nil || retained.Status != "cancelled" || retained.TerminalAt == nil {
		t.Fatalf("cancelled: %+v %v", retained, err)
	}
	got, _ = session.DigestJSON(retained.Input)
	if got != want {
		t.Fatalf("cancel lost input: %s", retained.Input)
	}
	sessions, err := session.ListSessions(t.Context(), f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Limit: 100})
	if err != nil || len(sessions.Sessions) == 0 {
		t.Fatalf("sessions: %+v %v", sessions, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE user_id=$1`, f.caller().ID)
	if _, err := session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("stale user: %v", err)
	}
}

func TestCompletedReadPublishesResultBeforeSave(t *testing.T) {
	f := newFixture(t)
	admission, _ := f.finalize(t, "prepared")
	turn, err := session.GetTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session, admission.TurnID)
	if err != nil || turn.Status != "completed" || len(turn.Result) == 0 {
		t.Fatalf("provisional result escaped: %+v %v", turn, err)
	}
}

func TestSessionDefinitionsRemainPinnedOutsideCurrentDeployment(t *testing.T) {
	f := newFixture(t)
	// A removal or promotion must not hide the open Session or retarget its links.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
	sessionID, err := session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session)
	if err != nil || sessionID.AgentDefinitionID != "agent" || sessionID.TemplateDefinitionID != "fixture-computer" || sessionID.DeploymentID != f.deployment {
		t.Fatalf("pinned definitions: %+v %v", sessionID, err)
	}
	page, err := session.ListSessions(t.Context(), f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Statuses: []string{"open"}, Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != f.session || page.Sessions[0].TemplateDefinitionID != "fixture-computer" {
		t.Fatalf("hidden old Session: %+v %v", page, err)
	}
}

func TestSessionAgentNameHistoryWithoutCurrentDeployment(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
	page, err := session.ListSessions(t.Context(), f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Limit: 50, Agent: "agent"})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != f.session {
		t.Fatalf("history query: %+v %v", page, err)
	}
	page, err = session.ListSessions(t.Context(), f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Limit: 50, Agent: "missing"})
	if err != nil || len(page.Sessions) != 0 {
		t.Fatalf("unknown agent: %+v %v", page, err)
	}

}
