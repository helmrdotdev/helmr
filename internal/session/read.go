package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func ReadEvents(ctx context.Context, q db.Querier, target Target, after int64, limit int32) (EventPage, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return EventPage{}, &OperationError{Code: "invalid_cursor"}
	}
	actor, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), ID: pgvalue.UUID(target.SessionID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return EventPage{}, &OperationError{Code: "session_not_found"}
	}
	if err != nil {
		return EventPage{}, err
	}
	if after >= actor.NextEventSequence {
		return EventPage{}, &OperationError{Code: "invalid_cursor"}
	}
	rows, err := q.ListSessionEvents(ctx, db.ListSessionEventsParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, AfterSequence: after, LimitCount: limit + 1})
	if err != nil {
		return EventPage{}, err
	}
	page := EventPage{Records: rows, NextAfter: after, HasMore: len(rows) > int(limit)}
	if page.HasMore {
		page.Records = rows[:limit]
	}
	if len(page.Records) > 0 {
		page.NextAfter = page.Records[len(page.Records)-1].Sequence
	}
	if page.Records == nil {
		page.Records = []db.ListSessionEventsRow{}
	}
	// Events currently retain the whole Session lifetime, so the retained prefix is zero.
	return page, nil
}

func GetTurn(ctx context.Context, q db.Querier, target Target, turnID uuid.UUID) (TurnView, error) {
	turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), SessionID: pgvalue.UUID(target.SessionID), ID: pgvalue.UUID(turnID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return TurnView{}, &OperationError{Code: "turn_not_found"}
	}
	if err != nil {
		return TurnView{}, err
	}
	view := TurnView{Turn: turn}
	view.AcceptsMessages, err = q.SessionTurnAcceptsMessages(ctx, db.SessionTurnAcceptsMessagesParams{EnvironmentID: turn.EnvironmentID, SessionID: turn.SessionID, ID: turn.ID})
	if err != nil {
		return TurnView{}, err
	}
	if turn.TerminalEventID.Valid {
		event, err := q.GetSessionEvent(ctx, db.GetSessionEventParams{EnvironmentID: turn.EnvironmentID, SessionID: turn.SessionID, ID: turn.TerminalEventID})
		if err != nil {
			return TurnView{}, err
		}
		view.TerminalEvent = &event
	}
	return view, nil
}
