package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// childWaitScope is the part of a locked execution a child wait is checked
// against.
type childWaitScope struct {
	run     db.Run
	session db.Session
}

func childWaitScopeOf(execution run.Execution) childWaitScope {
	return childWaitScope{run: execution.Run(), session: execution.Session()}
}

func validateChildWaitScope(scope childWaitScope, wait db.RunWait) error {
	if scope.run.EntrypointKind == "task" {
		if scope.run.SessionID.Valid || wait.TurnID.Valid || wait.TurnSessionID.Valid || wait.TurnRunGeneration.Valid {
			return run.ErrTurnScope
		}
		return nil
	}
	actor := scope.session
	if scope.run.EntrypointKind != "actor" || !actor.ID.Valid || scope.run.SessionID != actor.ID || actor.CurrentRunID != scope.run.ID || actor.ComputerID != scope.run.ComputerID || (actor.Status != "open" && actor.Status != "closing") {
		return run.ErrTurnScope
	}
	if actor.DispatchHoldID.Valid {
		return run.ErrTurnStopped
	}
	if actor.ActiveTurnID != wait.TurnID {
		return run.ErrTurnScope
	}
	if wait.TurnID.Valid && (wait.TurnSessionID != actor.ID || !wait.TurnRunGeneration.Valid || wait.TurnRunGeneration.Int64 != actor.RunGeneration) {
		return run.ErrTurnScope
	}
	if !wait.TurnID.Valid && (wait.TurnSessionID.Valid || wait.TurnRunGeneration.Valid) {
		return run.ErrTurnScope
	}
	return nil
}

func validateWorkerWaitTurn(ctx context.Context, tx pgx.Tx, a run.Execution, turnID pgtype.UUID, gen pgtype.Int8) error {
	if a.Run().EntrypointKind != "actor" {
		if turnID.Valid {
			return run.ErrTurnScope
		}
		return nil
	}
	if a.Session().DispatchHoldID.Valid {
		return run.ErrTurnStopped
	}
	if a.Session().ActiveTurnID != turnID {
		return run.ErrTurnScope
	}
	if !turnID.Valid {
		return nil
	}
	return a.ValidateTurnWork(ctx, tx, pgvalue.MustUUIDValue(turnID), gen.Int64)
}

// Registration already validated the parsed binding under the Session lock;
// the conditional write independently rejects a changed owner.
func bindWorkerWaitTurn(ctx context.Context, q db.Querier, a run.Execution, waitID, turnID pgtype.UUID, gen pgtype.Int8) error {
	if !turnID.Valid {
		return nil
	}
	_, err := q.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: a.Session().ID, TurnID: turnID, RunGeneration: gen, WaitID: waitID})
	return err
}
