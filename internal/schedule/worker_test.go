package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type testRunCreator struct {
	snapshotErr error
}

func (r testRunCreator) SnapshotScheduleFire(context.Context, db.ClaimDueScheduleInstancesRow) (FireSnapshot, error) {
	return FireSnapshot{}, r.snapshotErr
}

func (testRunCreator) CreateScheduleRun(context.Context, db.ClaimDueScheduleFiresRow, pgtype.UUID) (pgtype.UUID, error) {
	return pgtype.UUID{}, nil
}

type testDBTX struct {
	execCount int
}

func (db *testDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	db.execCount++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (*testDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}

func (*testDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	panic("unexpected query row")
}

func TestMaterializeInstanceLeavesDueWhenSnapshotFails(t *testing.T) {
	testDB := &testDBTX{}
	worker := &Worker{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:     db.New(testDB),
		runner: testRunCreator{snapshotErr: errors.New("snapshot failed")},
		now:    func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) },
	}
	err := worker.materializeInstance(context.Background(), db.ClaimDueScheduleInstancesRow{
		ScheduleID:      ids.ToPG(ids.New()),
		InstanceID:      ids.ToPG(ids.New()),
		Generation:      1,
		NextScheduledAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if testDB.execCount != 1 {
		t.Fatalf("delay updates = %d, want 1", testDB.execCount)
	}
}
