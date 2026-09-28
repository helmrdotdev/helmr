package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5/pgtype"
)

func validateChildWaitScope(authority run.ExecutionAuthority, wait db.RunWait) error {
	if authority.Run.EntrypointKind == "task" {
		if authority.Run.SessionID.Valid || wait.TurnID.Valid || wait.TurnSessionID.Valid || wait.TurnRunGeneration.Valid {
			return session.ErrTurnScope
		}
		return nil
	}
	actor := authority.Session
	if authority.Run.EntrypointKind != "actor" || !actor.ID.Valid || authority.Run.SessionID != actor.ID || actor.CurrentRunID != authority.Run.ID || actor.ComputerID != authority.Run.ComputerID || (actor.Status != "open" && actor.Status != "closing") {
		return session.ErrTurnScope
	}
	if actor.DispatchHoldID.Valid {
		return session.ErrTurnStopped
	}
	if actor.ActiveTurnID != wait.TurnID {
		return session.ErrTurnScope
	}
	if wait.TurnID.Valid && (wait.TurnSessionID != actor.ID || !wait.TurnRunGeneration.Valid || wait.TurnRunGeneration.Int64 != actor.RunGeneration) {
		return session.ErrTurnScope
	}
	if !wait.TurnID.Valid && (wait.TurnSessionID.Valid || wait.TurnRunGeneration.Valid) {
		return session.ErrTurnScope
	}
	return nil
}

func validateWorkerWaitTurn(ctx context.Context, q db.Querier, a run.ExecutionAuthority, turnID pgtype.UUID, gen pgtype.Int8) error {
	if a.Run.EntrypointKind != "actor" {
		if turnID.Valid {
			return session.ErrTurnScope
		}
		return nil
	}
	if a.Session.DispatchHoldID.Valid {
		return session.ErrTurnStopped
	}
	if a.Session.ActiveTurnID != turnID {
		return session.ErrTurnScope
	}
	if !turnID.Valid {
		return nil
	}
	_, err := session.ValidateTurnWork(ctx, q, session.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(a.Session.EnvironmentID), SessionID: pgvalue.MustUUIDValue(a.Session.ID), RunID: pgvalue.MustUUIDValue(a.Run.ID), TurnID: pgvalue.MustUUIDValue(turnID), AttemptNumber: a.Attempt.Number, RunGeneration: gen.Int64})
	return err
}

// Registration already validated the parsed binding under the Session lock;
// the conditional write independently rejects a changed owner.
func bindWorkerWaitTurn(ctx context.Context, q db.Querier, a run.ExecutionAuthority, waitID, turnID pgtype.UUID, gen pgtype.Int8) error {
	if !turnID.Valid {
		return nil
	}
	_, err := q.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: a.Session.ID, TurnID: turnID, RunGeneration: gen, WaitID: waitID})
	return err
}
