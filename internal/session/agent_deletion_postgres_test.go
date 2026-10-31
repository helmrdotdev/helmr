package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
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
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,false,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,root_session_id,parent_session_id,requester_session_id,causal_depth,origin_user_id,status,close_how) VALUES($1,$4,$2,$5,$6,$6,$6,1,$7,'closed','close');`, pgx.QueryExecModeSimpleProtocol, f.env, target, f.agent, child, f.deployment, f.session, f.user)
	sessiontest.PublishRuntimeAgentSet(t, sessiontest.Fixture{Pool: f.pool, Environment: f.env, Deployment: f.deployment}, "agent", "child-agent")
	turn := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET computer_id=$3 WHERE environment_id=$1 AND id=$2; INSERT INTO turns(environment_id,id,session_id,computer_id,seq,status,caller_kind,caller_id,admission_method,target_id,request_digest,terminal_at,processing_closed_at) VALUES($1,$4,$2,$3,1,'cancelled','session',$5,'spawn',$2,decode(repeat('00',32),'hex'),clock_timestamp(),clock_timestamp())`, pgx.QueryExecModeSimpleProtocol, f.env, child, f.computer, turn, f.session)
	if _, err := session.RuntimeObserveTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, child, turn); err != nil {
		t.Fatalf("runtime historical outcome: %v", err)
	}
	if _, err := session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, child); err != nil {
		t.Fatalf("human historical read: %v", err)
	}
	req := f.startRequest("deleted-child")
	req.Agent = "child-agent"
	req.ComputerID = f.computer
	if _, err := session.AdmitSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req, "spawn"); !errors.Is(err, session.ErrAgentDeleted) {
		t.Fatalf("deleted pinned child admitted: %v", err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, session.SessionControlRequest{EnvironmentID: f.env, SessionID: child, Kind: "cancel", IdempotencyKey: "stop-deleted-child"}); err != nil {
		t.Fatalf("runtime stop of settled deleted child: %v", err)
	}
	otherUser := uuid.NewV7()
	var orgID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'unrelated')`, otherUser)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, uuid.NewV7(), orgID, otherUser)
	dbtest.GrantMember(t, f.pool, orgID, otherUser, &f.env, "sessions.create")
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, session.Caller{Kind: "user", ID: otherUser, CredentialID: uuid.NewV7()}, session.EnqueueRequest{EnvironmentID: f.env, SessionID: child, IdempotencyKey: "unrelated", Input: []byte(`[{"type":"text","text":"unrelated"}]`)}); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("unauthorized caller learned deletion state: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE role_grants SET all_agents=false WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, otherUser)
	if _, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, session.Caller{Kind: "user", ID: otherUser, CredentialID: uuid.NewV7()}, req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("unauthorized start learned deletion state: %v", err)
	}
	held, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "hold"))
	if err != nil {
		t.Fatal(err)
	}
	resume := controlRequest(f, "resume", "resume")
	resume.HoldID = held.HoldID
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); err != nil {
		t.Fatalf("deleted settled child blocks parent resume: %v", err)
	}
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
		t.Fatalf("parent cancellation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agents SET deleted_at=NULL,revision=revision+1 WHERE environment_id=$1 AND id=$2`, f.env, target)
	// Runtime execution identity changed with the parent controls; use an external
	// root admission to verify the restored target becomes available again.
	if _, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); err != nil {
		t.Fatalf("restored Agent unavailable: %v", err)
	}

}

func TestAgentDeletionCancelsSessionsAndCleanupContinues(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	queued := f.enqueue(t, "queued")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
	var orgID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	dbtest.GrantMember(t, f.pool, orgID, f.user, nil, "assignments.create")
	dbtest.GrantMember(t, f.pool, orgID, f.user, &f.env, "agents.delete")
	principal, err := access.HumanInEnvironment(ctx, f.pool, f.human, f.env)
	if err != nil {
		t.Fatal(err)
	}
	scope := auth.Scope{OrgID: principal.OrgID, ProjectID: principal.ProjectID, EnvironmentID: principal.EnvironmentID}
	preview, err := deployment.ChangeAgent(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, scope, "agent", "", false, true)
	if err != nil || !preview.CanApply || len(preview.Blockers) != 0 || preview.Effects.SessionsCancelled != 1 {
		t.Fatalf("preview %+v %v", preview, err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id=$1`, f.session).Scan(&status); err != nil || status != "open" {
		t.Fatalf("preview changed the Session: %s %v", status, err)
	}
	deleted, err := deployment.ChangeAgent(ctx, f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, scope, "agent", "1", false, false)
	if err != nil || !deleted.Changed || deleted.Effects.SessionsCancelled != 1 {
		t.Fatalf("delete %+v %v", deleted, err)
	}
	var how, turn string
	var stopping bool
	if err := f.pool.QueryRow(ctx, `SELECT s.status,s.close_how,(SELECT status FROM turns WHERE id=$2),EXISTS(SELECT 1 FROM session_processes p WHERE p.session_id=s.id AND p.status='stopping' AND p.fenced_at IS NULL)
 FROM sessions s WHERE s.id=$1`, f.session, queued.TurnID).Scan(&status, &how, &turn, &stopping); err != nil || status != "closed" || how != "cancel" || turn != "cancelled" || !stopping {
		t.Fatalf("deleted Agent Session: %s %s %s stopping=%v %v", status, how, turn, stopping, err)
	}
	// Background cleanup still settles the process of a deleted Agent's Session.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='deleted Agent closure' WHERE computer_id=$1 AND fenced_at IS NULL`, f.computer)
	if err := session.ReconcileTerminalSessionStop(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var fenced bool
	if err := f.pool.QueryRow(ctx, `SELECT bool_and(fenced_at IS NOT NULL AND status='stopped') FROM session_processes WHERE session_id=$1`, f.session).Scan(&fenced); err != nil || !fenced {
		t.Fatalf("deleted Agent process cleanup: %v %v", fenced, err)
	}
	// A repeated cancel of the closed Session records nothing.
	before := countRows(t, f, f.session)
	if receipt, err := session.ControlSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "cancel", "after-delete")); !errors.Is(err, session.ErrAgentDeleted) && (err != nil || receipt.Status != "closed") {
		t.Fatalf("cancel after delete: %+v %v", receipt, err)
	}
	if after := countRows(t, f, f.session); after != before {
		t.Fatalf("cancel after delete wrote rows: %v -> %v", before, after)
	}
}

func TestAgentDeleteSerializesPinnedAndScheduledAdmission(t *testing.T) {
	for _, kind := range []string{"spawn", "schedule"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			caller := runtimeReadCaller(t, f)
			target := uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'child-agent'); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,false,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3; UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, target, f.agent)
			sessiontest.PublishRuntimeAgentSet(t, sessiontest.Fixture{Pool: f.pool, Environment: f.env, Deployment: f.deployment}, "agent", "child-agent")
			var orgID uuid.UUID
			if err := f.pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&orgID); err != nil {
				t.Fatal(err)
			}
			dbtest.GrantMember(t, f.pool, orgID, f.user, nil, "assignments.create")
			dbtest.GrantMember(t, f.pool, orgID, f.user, &f.env, "agents.delete")
			principal, err := access.HumanInEnvironment(t.Context(), f.pool, f.human, f.env)
			if err != nil {
				t.Fatal(err)
			}
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
				_, err := deployment.ChangeAgent(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, scope, "child-agent", "1", false, false)
				deleted <- err
			}()
			go func() {
				<-start
				if kind == "schedule" {
					admitted <- agent.EvaluateTrigger(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.env, activation)
					return
				}
				req := f.startRequest("racing-child")
				req.Agent = "child-agent"
				req.ComputerID = f.computer
				_, err := session.AdmitSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req, "spawn")
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
			// Deletion either precedes admission, or cancels the admitted Session.
			var isDeleted bool
			var sessions int
			if err := f.pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL,(SELECT count(*) FROM sessions WHERE environment_id=$1 AND agent_id=$2 AND status<>'closed') FROM agents WHERE environment_id=$1 AND id=$2`, f.env, target).Scan(&isDeleted, &sessions); err != nil {
				t.Fatal(err)
			}
			if (isDeleted && sessions != 0) || (!isDeleted && sessions != 1) {
				t.Fatalf("race left deleted=%v sessions=%d errors=%v/%v", isDeleted, sessions, deletionErr, admissionErr)
			}
			if isDeleted && kind == "schedule" {
				var before int
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE environment_id=$1 AND agent_id=$2`, f.env, target).Scan(&before); err != nil {
					t.Fatal(err)
				}
				if _, err := deployment.ChangeAgent(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, scope, "child-agent", "2", true, false); err != nil {
					t.Fatal(err)
				}
				if err := agent.EvaluateTrigger(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.env, activation); err != nil {
					t.Fatal(err)
				}
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE environment_id=$1 AND agent_id=$2`, f.env, target).Scan(&sessions); err != nil || sessions != before {
					t.Fatalf("restore replayed schedule: %d %v", sessions, err)
				}
			}
		})
	}
}

func TestAgentCleanupSerializesSameManagerBeforeSharedChecks(t *testing.T) {
	f := newAdmissionFixture(t)
	principal, err := access.HumanInEnvironment(t.Context(), f.pool, f.human, f.env)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.GrantMember(t, f.pool, principal.OrgID, f.user, &f.env, "agents.delete")
	scope := auth.Scope{OrgID: principal.OrgID, ProjectID: principal.ProjectID, EnvironmentID: principal.EnvironmentID}
	gate, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(t.Context())
	var member uuid.UUID
	if err = gate.QueryRow(t.Context(), `SELECT id FROM org_members WHERE id=$1 FOR SHARE`, principal.MemberID).Scan(&member); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := deployment.ChangeAgent(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, scope, "agent", "1", false, true)
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%org_members%' AND query LIKE '%FOR UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("management calls did not reach membership lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = gate.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = <-results; err != nil {
			t.Fatal("concurrent cleanup failed", err)
		}
	}
}
