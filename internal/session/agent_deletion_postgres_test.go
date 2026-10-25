package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
	"uuid"
)

func TestDeletedChildAgentPreservesHistoryAndParentControls(t *testing.T) {
	f := newAdmissionFixture(t)
	caller := runtimeReadCaller(t, f)
	target, child := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key,deleted_at) VALUES($1,$2,'child-agent',clock_timestamp());
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) SELECT environment_id,$2,deployment_id,template_id,false FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,root_session_id,parent_session_id,requester_session_id,causal_depth,origin_user_id,status) VALUES($1,$4,$2,$5,$6,$6,$6,1,$7,'closed');`, pgx.QueryExecModeSimpleProtocol, f.env, target, f.agent, child, f.deployment, f.session, f.user)
	turn := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET computer_id=$3 WHERE environment_id=$1 AND id=$2; INSERT INTO turns(environment_id,id,session_id,computer_id,seq,status,caller_kind,caller_id,admission_method,target_id,request_digest,terminal_at,processing_closed_at) VALUES($1,$4,$2,$3,1,'cancelled','session',$5,'spawn',$2,decode(repeat('00',32),'hex'),clock_timestamp(),clock_timestamp())`, pgx.QueryExecModeSimpleProtocol, f.env, child, f.computer, turn, f.session)
	if _, err := session.RuntimeObserveTurn(t.Context(), f.pool, caller, child, turn); err != nil {
		t.Fatalf("runtime historical outcome: %v", err)
	}
	if _, err := session.GetSession(t.Context(), f.pool, f.caller(), f.env, child); err != nil {
		t.Fatalf("human historical read: %v", err)
	}
	req := f.startRequest("deleted-child")
	req.Agent = "child-agent"
	req.ComputerID = f.computer
	if _, err := session.AdmitSession(t.Context(), f.pool, nil, caller, req, "spawn"); !errors.Is(err, session.ErrAgentDeleted) {
		t.Fatalf("deleted pinned child admitted: %v", err)
	}
	if _, err := session.ControlSession(t.Context(), f.pool, caller, session.SessionControlRequest{EnvironmentID: f.env, SessionID: child, Kind: "cancel", IdempotencyKey: "stop-deleted-child"}); err != nil {
		t.Fatalf("runtime stop of settled deleted child: %v", err)
	}
	otherUser := uuid.NewV7()
	var orgID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'unrelated')`, otherUser)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, uuid.NewV7(), orgID, otherUser)
	dbtest.GrantMember(t, f.pool, orgID, otherUser, &f.env, "agents.use")
	if _, err := session.Enqueue(t.Context(), f.pool, nil, session.Caller{Kind: "user", ID: otherUser}, session.EnqueueRequest{EnvironmentID: f.env, SessionID: child, IdempotencyKey: "unrelated", Input: []byte(`[{"type":"text","text":"unrelated"}]`)}); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("unauthorized caller learned deletion state: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE role_grants SET all_agents=false WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, otherUser)
	if _, err := session.Start(t.Context(), f.pool, nil, session.Caller{Kind: "user", ID: otherUser}, req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("unauthorized start learned deletion state: %v", err)
	}
	held, err := session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "hold"))
	if err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(f, "resume", "resume")
	resume.HoldID = held.HoldID
	if _, err = session.ControlSession(t.Context(), f.pool, f.caller(), resume); err != nil {
		t.Fatalf("deleted settled child blocks parent resume: %v", err)
	}
	if _, err = session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
		t.Fatalf("parent cancellation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agents SET deleted_at=NULL,revision=revision+1 WHERE environment_id=$1 AND id=$2`, f.env, target)
	// Runtime execution identity changed with the parent controls; use an external
	// root admission to verify the restored target becomes available again.
	if _, err := session.Start(t.Context(), f.pool, nil, f.caller(), req); err != nil {
		t.Fatalf("restored Agent unavailable: %v", err)
	}

}

func TestAgentDeletionWaitsForTerminalSessionProcessCleanup(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled' WHERE environment_id=$1 AND id=$2`, f.env, f.session)
	err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		blockers, err := session.AgentDeletionBlockers(t.Context(), tx, f.env, f.agent, true)
		if err != nil {
			return err
		}
		if len(blockers) != 1 || blockers[0].Code != "cleanup_pending" || blockers[0].Count != 1 {
			t.Fatalf("unfenced cleanup: %+v", blockers)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, f.session)
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		blockers, err := session.AgentDeletionBlockers(t.Context(), tx, f.env, f.agent, true)
		if err == nil && len(blockers) != 0 {
			t.Fatalf("fenced process still blocks deletion: %+v", blockers)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

}

func TestAgentDeleteSerializesPinnedAndScheduledAdmission(t *testing.T) {
	for _, kind := range []string{"spawn", "schedule"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			caller := runtimeReadCaller(t, f)
			target := uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'child-agent'); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) SELECT environment_id,$2,deployment_id,template_id,false FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3; UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, target, f.agent)
			var orgID uuid.UUID
			if err := f.pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&orgID); err != nil {
				t.Fatal(err)
			}
			dbtest.GrantMember(t, f.pool, orgID, f.user, nil, "access.manage")
			dbtest.GrantMember(t, f.pool, orgID, f.user, &f.env, "agents.manage")
			principal, err := access.Principal(t.Context(), f.pool, "user", f.user, f.env)
			if err != nil {
				t.Fatal(err)
			}
			principal.SessionID = uuid.NewV7()
			scope := auth.Scope{OrgID: principal.OrgID, ProjectID: principal.ProjectID, EnvironmentID: principal.EnvironmentID}
			activation := uuid.NewV7()
			if kind == "schedule" {
				due := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) VALUES($1,$2,$3,'daily','* * * * *','UTC','[{"text":"scheduled","type":"text"}]'); INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,active_until,next_fire_at) VALUES($1,$4,$2,$3,'daily',$5,$6,$5)`, pgx.QueryExecModeSimpleProtocol, f.env, target, f.deployment, activation, due, due.Add(time.Minute))
			}
			start := make(chan struct{})
			deleted := make(chan error, 1)
			admitted := make(chan error, 1)
			go func() {
				<-start
				_, err := deployment.ChangeAgent(t.Context(), f.pool, principal, scope, "child-agent", "1", false, false)
				deleted <- err
			}()
			go func() {
				<-start
				if kind == "schedule" {
					admitted <- agent.EvaluateTrigger(t.Context(), f.pool, nil, f.env, activation)
					return
				}
				req := f.startRequest("racing-child")
				req.Agent = "child-agent"
				req.ComputerID = f.computer
				_, err := session.AdmitSession(t.Context(), f.pool, nil, caller, req, "spawn")
				admitted <- err
			}()
			close(start)
			deletionErr, admissionErr := <-deleted, <-admitted
			if deletionErr != nil && !errors.Is(deletionErr, deployment.ErrAgentDeletionBlocked) {
				t.Fatal(deletionErr)
			}
			if admissionErr != nil && !errors.Is(admissionErr, session.ErrAgentDeleted) {
				t.Fatal(admissionErr)
			}
			var isDeleted bool
			var sessions int
			if err := f.pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL,(SELECT count(*) FROM sessions WHERE environment_id=$1 AND agent_id=$2) FROM agents WHERE environment_id=$1 AND id=$2`, f.env, target).Scan(&isDeleted, &sessions); err != nil {
				t.Fatal(err)
			}
			if (isDeleted && sessions != 0) || (!isDeleted && sessions != 1) {
				t.Fatalf("race left deleted=%v sessions=%d errors=%v/%v", isDeleted, sessions, deletionErr, admissionErr)
			}
			if isDeleted && kind == "schedule" {
				if _, err := deployment.ChangeAgent(t.Context(), f.pool, principal, scope, "child-agent", "2", true, false); err != nil {
					t.Fatal(err)
				}
				if err := agent.EvaluateTrigger(t.Context(), f.pool, nil, f.env, activation); err != nil {
					t.Fatal(err)
				}
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE environment_id=$1 AND agent_id=$2`, f.env, target).Scan(&sessions); err != nil || sessions != 0 {
					t.Fatalf("restore replayed schedule: %d %v", sessions, err)
				}
			}
		})
	}
}
