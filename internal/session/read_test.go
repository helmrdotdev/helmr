package session

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type snapshotStore struct {
	snapshot  db.GetSessionSnapshotRow
	keyed     db.GetSessionSnapshotByKeyRow
	err       error
	rows      []db.ListSessionSnapshotsRow
	requested db.GetSessionSnapshotParams
	byKey     db.GetSessionSnapshotByKeyParams
	listed    db.ListSessionSnapshotsParams
}

func (s *snapshotStore) GetSessionSnapshot(_ context.Context, arg db.GetSessionSnapshotParams) (db.GetSessionSnapshotRow, error) {
	s.requested = arg
	return s.snapshot, s.err
}

func (s *snapshotStore) GetSessionSnapshotByKey(_ context.Context, arg db.GetSessionSnapshotByKeyParams) (db.GetSessionSnapshotByKeyRow, error) {
	s.byKey = arg
	return s.keyed, s.err
}

func (s *snapshotStore) ListSessionSnapshots(_ context.Context, arg db.ListSessionSnapshotsParams) ([]db.ListSessionSnapshotsRow, error) {
	s.listed = arg
	if int(arg.LimitCount) < len(s.rows) {
		return s.rows[:arg.LimitCount], nil
	}
	return s.rows, nil
}

func TestGetReadsTheScopedSnapshot(t *testing.T) {
	scope := Scope{OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7())}
	sessionID := pgvalue.UUID(uuid.NewV7())
	store := &snapshotStore{snapshot: db.GetSessionSnapshotRow{ID: sessionID}}
	row, err := Get(t.Context(), store, scope, sessionID)
	if err != nil || row.ID != sessionID || store.requested != (db.GetSessionSnapshotParams{OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, ID: sessionID}) {
		t.Fatalf("Get = %+v %v requested %+v", row, err, store.requested)
	}
	store.err = pgx.ErrNoRows
	if _, err := Get(t.Context(), store, scope, sessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Session error = %v", err)
	}
	unavailable := errors.New("database is down")
	store.err = unavailable
	if _, err := Get(t.Context(), store, scope, sessionID); !errors.Is(err, unavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("failed read error = %v", err)
	}
}

func TestGetByKeyReadsTheScopedKey(t *testing.T) {
	scope := Scope{OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7())}
	sessionID := pgvalue.UUID(uuid.NewV7())
	store := &snapshotStore{keyed: db.GetSessionSnapshotByKeyRow{ID: sessionID}}
	row, err := GetByKey(t.Context(), store, scope, "operator.v1", "thread:1")
	if err != nil || row.ID != sessionID || store.byKey != (db.GetSessionSnapshotByKeyParams{OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, ActorDeclaredID: "operator.v1", Key: pgvalue.Text("thread:1")}) {
		t.Fatalf("GetByKey = %+v %v requested %+v", row, err, store.byKey)
	}
	store.err = pgx.ErrNoRows
	if _, err := GetByKey(t.Context(), store, scope, "operator.v1", "thread:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key error = %v", err)
	}
}

func TestListReadsOneExtraRowToReportMore(t *testing.T) {
	store := &snapshotStore{rows: make([]db.ListSessionSnapshotsRow, 3)}
	for n := range store.rows {
		store.rows[n].ID = pgvalue.UUID(uuid.NewV7())
	}
	query := ListQuery{Statuses: []string{"open"}, AfterID: pgvalue.UUID(uuid.NewV7()), Limit: 2}
	rows, more, err := List(t.Context(), store, Scope{}, query)
	if err != nil || !more || len(rows) != 2 || store.listed.LimitCount != 3 || store.listed.AfterID != query.AfterID || store.listed.Statuses[0] != "open" {
		t.Fatalf("List = %d rows more=%v %v listed %+v", len(rows), more, err, store.listed)
	}
	query.Limit = 3
	if rows, more, err := List(t.Context(), store, Scope{}, query); err != nil || more || len(rows) != 3 {
		t.Fatalf("last page = %d rows more=%v %v", len(rows), more, err)
	}
}
