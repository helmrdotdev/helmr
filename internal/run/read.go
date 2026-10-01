package run

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrNotFound reports a Run the scope does not have.
var ErrNotFound = errors.New("run not found")

// ReadStore reads Run snapshots and list pages.
type ReadStore interface {
	GetRunSnapshot(context.Context, db.GetRunSnapshotParams) (db.GetRunSnapshotRow, error)
	ListRunListItems(context.Context, db.ListRunListItemsParams) ([]db.ListRunListItemsRow, error)
}

// Scope is the organization, project and environment a Run read addresses.
type Scope struct {
	OrgID, ProjectID, EnvironmentID pgtype.UUID
}

// Get reads one Run's snapshot without a transaction. A Run outside the
// scope is ErrNotFound.
func Get(ctx context.Context, store ReadStore, scope Scope, runID pgtype.UUID) (db.GetRunSnapshotRow, error) {
	row, err := store.GetRunSnapshot(ctx, db.GetRunSnapshotParams{
		OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, ID: runID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetRunSnapshotRow{}, ErrNotFound
	}
	return row, err
}

// ListQuery filters and pages a Run list. Empty filters match every Run; a
// cursor resumes after the Run it names.
type ListQuery struct {
	Statuses       []db.RunStatus
	Kinds          []string
	SessionID      pgtype.UUID
	AfterCreatedAt pgtype.Timestamptz
	AfterID        pgtype.UUID
	Limit          int32
}

// List reads, without a transaction, up to query.Limit Runs of the scope and
// whether more follow.
func List(ctx context.Context, store ReadStore, scope Scope, query ListQuery) ([]db.ListRunListItemsRow, bool, error) {
	rows, err := store.ListRunListItems(ctx, db.ListRunListItemsParams{
		OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
		Statuses: query.Statuses, EntrypointKinds: query.Kinds, SessionID: query.SessionID,
		AfterCreatedAt: query.AfterCreatedAt, AfterID: query.AfterID, LimitCount: query.Limit + 1,
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
