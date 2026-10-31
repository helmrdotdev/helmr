package session_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestComputerDeletionCancelsItsSessionsAndCommands(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	child := ownedFixture(t, f)
	queued := f.enqueue(t, "queued")
	childTurn := child.enqueue(t, "child")
	if _, err := dispatchTurnForTest(ctx, f.pool, child.execution()); err != nil {
		t.Fatal(err)
	}
	hold := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, hold, f.session)
	command := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO commands(environment_id,id,computer_id,argv,cwd,env,stdin,timeout_ms,caller_kind,caller_id)
 VALUES($1,$2,$3,ARRAY['true'],'/workspace','{}','',1000,'user',$4)`, f.env, command, f.computer, f.user)
	scope := computer.Scope{EnvironmentID: f.env}
	if err := f.pool.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, f.env).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	deletion := computer.Deletion{Principal: sessiontestPrincipal(t, f), Scope: scope, ComputerID: f.computer, IdempotencyKey: "delete",
		CancelSessions: session.ComputerSessionCancellation(policy)}
	preview := deletion
	preview.DryRun = true
	counted, err := computer.Delete(ctx, policy, f.pool, preview)
	if err != nil || counted.SessionsCancelled != 2 || counted.CommandsCancelled != 1 {
		t.Fatalf("preview %+v %v", counted, err)
	}
	var deletedAt *string
	if err := f.pool.QueryRow(ctx, `SELECT deleted_at::text FROM computers WHERE id=$1`, f.computer).Scan(&deletedAt); err != nil || deletedAt != nil {
		t.Fatalf("preview deleted the Computer: %v %v", deletedAt, err)
	}
	deleted, err := computer.Delete(ctx, policy, f.pool, deletion)
	if err != nil || deleted.SessionsCancelled != 2 || deleted.CommandsCancelled != 1 {
		t.Fatalf("delete %+v %v", deleted, err)
	}
	var parent, owned, parentTurn, ownedTurn, commandStatus string
	var released, deleting bool
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT status FROM sessions WHERE id=$1),(SELECT status FROM sessions WHERE id=$2),(SELECT status FROM turns WHERE id=$3),(SELECT status FROM turns WHERE id=$4),
 (SELECT status FROM commands WHERE id=$5),(SELECT released_at IS NOT NULL FROM session_holds WHERE id=$6),(SELECT deleted_at IS NOT NULL FROM computers WHERE id=$7)`,
		f.session, child.session, queued.TurnID, childTurn.TurnID, command, hold, f.computer).Scan(&parent, &owned, &parentTurn, &ownedTurn, &commandStatus, &released, &deleting); err != nil {
		t.Fatal(err)
	}
	if parent != "closed" || owned != "closed" || parentTurn != "cancelled" || ownedTurn != "cancelled" || commandStatus != "cancelled" || !released || !deleting {
		t.Fatalf("deletion left %s %s %s %s %s released=%v deleting=%v", parent, owned, parentTurn, ownedTurn, commandStatus, released, deleting)
	}
	replay, err := computer.Delete(ctx, policy, f.pool, deletion)
	if err != nil || !replay.Replayed || replay.SessionsCancelled != 2 {
		t.Fatalf("replay %+v %v", replay, err)
	}
}

func TestComputerDeletionRacesLeaveNothingLive(t *testing.T) {
	for i := range 3 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := newAdmissionFixture(t)
			ctx := t.Context()
			policy := auditpolicy.Policy{RetentionSeconds: 3600}
			scope := computer.Scope{EnvironmentID: f.env}
			if err := f.pool.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, f.env).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
				t.Fatal(err)
			}
			principal := sessiontestPrincipal(t, f)
			deletion := computer.Deletion{Principal: principal, Scope: scope, ComputerID: f.computer, IdempotencyKey: "delete", CancelSessions: session.ComputerSessionCancellation(policy)}
			start := make(chan struct{})
			results := make(chan error, 4)
			race := func(run func() error) {
				go func() {
					<-start
					results <- run()
				}()
			}
			race(func() error { _, err := computer.Delete(ctx, policy, f.pool, deletion); return err })
			race(func() error {
				req := f.startRequest("racing-start")
				req.ComputerID = f.computer
				_, err := session.Start(ctx, policy, f.pool, nil, f.caller(), req)
				return err
			})
			race(func() error {
				_, err := session.Enqueue(ctx, policy, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "racing-enqueue", Input: json.RawMessage(`[{"type":"text","text":"race"}]`)})
				return err
			})
			race(func() error {
				_, err := command.Create(ctx, policy, f.pool, command.CreateRequest{OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: f.env, ComputerID: f.computer, Creator: command.Creator{Human: principal, Kind: string(auth.PrincipalKindSession), ID: principal.SessionID, UserID: f.user}, Argv: []string{"true"}, IdempotencyKey: "racing-command"})
				return err
			})
			close(start)
			for range 4 {
				// Work admitted before the delete is cancelled by it; work after it is refused.
				if err := <-results; err != nil && !errors.Is(err, computer.ErrDeleting) && !errors.Is(err, computer.ErrNotReady) && !errors.Is(err, session.ErrNotReady) && !errors.Is(err, session.ErrTerminal) {
					t.Fatalf("racing operation: %v", err)
				}
			}
			var deleted bool
			var sessions, turns, commands int
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT deleted_at IS NOT NULL FROM computers WHERE id=$1),
 (SELECT count(*) FROM sessions WHERE computer_id=$1 AND status<>'closed'),
 (SELECT count(*) FROM turns WHERE computer_id=$1 AND status IN ('queued','running')),
 (SELECT count(*) FROM commands WHERE computer_id=$1 AND terminal_at IS NULL AND status<>'stopping')`, f.computer).Scan(&deleted, &sessions, &turns, &commands); err != nil {
				t.Fatal(err)
			}
			if !deleted || sessions != 0 || turns != 0 || commands != 0 {
				t.Fatalf("deleted=%v live sessions=%d turns=%d commands=%d", deleted, sessions, turns, commands)
			}
		})
	}
}
