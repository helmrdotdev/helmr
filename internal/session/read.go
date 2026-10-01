package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func ReadEvents(ctx context.Context, q db.Querier, target Target, after int64, limit int32) (EventPage, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return EventPage{}, &OperationError{Code: "invalid_cursor"}
	}
	actor, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), ID: pgvalue.UUID(target.SessionID)})
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

// ErrNotFound reports a Session the scope does not have.
var ErrNotFound = errors.New("session not found")

// SnapshotStore reads Session snapshots and list pages.
type SnapshotStore interface {
	GetSessionSnapshot(context.Context, db.GetSessionSnapshotParams) (db.GetSessionSnapshotRow, error)
	GetSessionSnapshotByKey(context.Context, db.GetSessionSnapshotByKeyParams) (db.GetSessionSnapshotByKeyRow, error)
	ListSessionSnapshots(context.Context, db.ListSessionSnapshotsParams) ([]db.ListSessionSnapshotsRow, error)
}

// Scope is the organization, project and environment a Session read
// addresses.
type Scope struct {
	OrgID, ProjectID, EnvironmentID pgtype.UUID
}

// Get reads one Session's snapshot without a transaction. A Session outside
// the scope is ErrNotFound.
func Get(ctx context.Context, store SnapshotStore, scope Scope, sessionID pgtype.UUID) (db.GetSessionSnapshotRow, error) {
	row, err := store.GetSessionSnapshot(ctx, db.GetSessionSnapshotParams{
		OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, ID: sessionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetSessionSnapshotRow{}, ErrNotFound
	}
	return row, err
}

// GetByKey reads, without a transaction, the snapshot of the Session an
// Actor's key names. No such Session in the scope is ErrNotFound.
func GetByKey(ctx context.Context, store SnapshotStore, scope Scope, actorDeclaredID, key string) (db.GetSessionSnapshotByKeyRow, error) {
	row, err := store.GetSessionSnapshotByKey(ctx, db.GetSessionSnapshotByKeyParams{
		OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
		ActorDeclaredID: actorDeclaredID, Key: pgvalue.Text(key),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetSessionSnapshotByKeyRow{}, ErrNotFound
	}
	return row, err
}

// ListQuery filters and pages a Session list. No statuses match every
// Session; a cursor resumes after the Session it names.
type ListQuery struct {
	Statuses       []string
	AfterCreatedAt pgtype.Timestamptz
	AfterID        pgtype.UUID
	Limit          int32
}

// List reads, without a transaction, up to query.Limit Sessions of the scope
// and whether more follow.
func List(ctx context.Context, store SnapshotStore, scope Scope, query ListQuery) ([]db.ListSessionSnapshotsRow, bool, error) {
	rows, err := store.ListSessionSnapshots(ctx, db.ListSessionSnapshotsParams{
		OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
		Statuses: query.Statuses, AfterCreatedAt: query.AfterCreatedAt, AfterID: query.AfterID,
		LimitCount: query.Limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > int(query.Limit)
	if hasMore {
		rows = rows[:query.Limit]
	}
	return rows, hasMore, nil
}
