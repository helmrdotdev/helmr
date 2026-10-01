package run

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type readStore struct {
	snapshot  db.GetRunSnapshotRow
	getErr    error
	rows      []db.ListRunListItemsRow
	listed    db.ListRunListItemsParams
	requested db.GetRunSnapshotParams
}

func (s *readStore) GetRunSnapshot(_ context.Context, arg db.GetRunSnapshotParams) (db.GetRunSnapshotRow, error) {
	s.requested = arg
	return s.snapshot, s.getErr
}

func (s *readStore) ListRunListItems(_ context.Context, arg db.ListRunListItemsParams) ([]db.ListRunListItemsRow, error) {
	s.listed = arg
	if int(arg.LimitCount) < len(s.rows) {
		return s.rows[:arg.LimitCount], nil
	}
	return s.rows, nil
}

func TestGetReadsTheScopedSnapshot(t *testing.T) {
	scope := Scope{OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7())}
	runID := pgvalue.UUID(uuid.NewV7())
	store := &readStore{snapshot: db.GetRunSnapshotRow{ID: runID}}
	row, err := Get(t.Context(), store, scope, runID)
	if err != nil || row.ID != runID || store.requested != (db.GetRunSnapshotParams{OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, ID: runID}) {
		t.Fatalf("Get = %+v %v requested %+v", row, err, store.requested)
	}
	store.getErr = pgx.ErrNoRows
	if _, err := Get(t.Context(), store, scope, runID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Run error = %v", err)
	}
	unavailable := errors.New("database is down")
	store.getErr = unavailable
	if _, err := Get(t.Context(), store, scope, runID); !errors.Is(err, unavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("failed read error = %v", err)
	}
}

func TestListReadsOneExtraRowToReportMore(t *testing.T) {
	store := &readStore{rows: make([]db.ListRunListItemsRow, 3)}
	for n := range store.rows {
		store.rows[n].ID = pgvalue.UUID(uuid.NewV7())
	}
	query := ListQuery{Statuses: []db.RunStatus{db.RunStatusQueued}, Kinds: []string{"task"}, AfterID: pgvalue.UUID(uuid.NewV7()), Limit: 2}
	rows, more, err := List(t.Context(), store, Scope{}, query)
	if err != nil || !more || len(rows) != 2 || store.listed.LimitCount != 3 || store.listed.AfterID != query.AfterID || store.listed.EntrypointKinds[0] != "task" {
		t.Fatalf("List = %d rows more=%v %v listed %+v", len(rows), more, err, store.listed)
	}
	query.Limit = 3
	if rows, more, err := List(t.Context(), store, Scope{}, query); err != nil || more || len(rows) != 3 {
		t.Fatalf("last page = %d rows more=%v %v", len(rows), more, err)
	}
}

type pollStore struct {
	wait       db.RunWait
	waitErr    error
	stopped    bool
	current    bool
	turnChecks int
}

func (s *pollStore) GetRunWait(context.Context, db.GetRunWaitParams) (db.RunWait, error) {
	return s.wait, s.waitErr
}

func (s *pollStore) RunWaitSessionStopped(context.Context, pgtype.UUID) (bool, error) {
	return s.stopped, nil
}

func (s *pollStore) RunWaitTurnCurrent(context.Context, pgtype.UUID) (bool, error) {
	s.turnChecks++
	return s.current, nil
}

func TestPollWaitChecksFenceSessionAndTurn(t *testing.T) {
	scope := WaitPollScope{RunID: pgvalue.UUID(uuid.NewV7()), AttemptNumber: 1, ComputerID: pgvalue.UUID(uuid.NewV7()), LeaseID: pgvalue.UUID(uuid.NewV7())}
	waitID := pgvalue.UUID(uuid.NewV7())
	wait := db.RunWait{ID: waitID, AttemptNumber: 1, ComputerID: scope.ComputerID, CurrentRunLeaseID: scope.LeaseID, SuspensionStatus: db.RunWaitStatusHot}
	for name, test := range map[string]struct {
		store   pollStore
		stopped bool
		turns   int
		want    error
	}{
		"current":        {store: pollStore{wait: wait, current: true}, turns: 1},
		"missing":        {store: pollStore{waitErr: pgx.ErrNoRows}, want: ErrWaitNotFound},
		"other computer": {store: pollStore{wait: func() db.RunWait { w := wait; w.ComputerID = pgvalue.UUID(uuid.NewV7()); return w }()}, want: ErrWaitFenceStale},
		"prior lease": {store: pollStore{wait: func() db.RunWait {
			w := wait
			w.CurrentRunLeaseID, w.PriorRunLeaseID = pgvalue.UUID(uuid.NewV7()), scope.LeaseID
			return w
		}(), current: true}, turns: 1},
		"other lease":     {store: pollStore{wait: func() db.RunWait { w := wait; w.CurrentRunLeaseID = pgvalue.UUID(uuid.NewV7()); return w }()}, want: ErrWaitFenceStale},
		"turn revoked":    {store: pollStore{wait: wait}, turns: 1, want: ErrWaitTurnRevoked},
		"stopped hot":     {store: pollStore{wait: wait, stopped: true, current: true}, turns: 1},
		"stopped release": {store: pollStore{wait: func() db.RunWait { w := wait; w.SuspensionStatus = db.RunWaitStatusReleased; return w }(), stopped: true}, stopped: true},
	} {
		t.Run(name, func(t *testing.T) {
			store := test.store
			got, stopped, err := PollWait(t.Context(), &store, scope, waitID)
			if !errors.Is(err, test.want) || (test.want == nil && (got.ID != waitID || stopped != test.stopped)) || store.turnChecks != test.turns {
				t.Fatalf("PollWait = %+v stopped=%v %v turn checks=%d", got, stopped, err, store.turnChecks)
			}
		})
	}
}
