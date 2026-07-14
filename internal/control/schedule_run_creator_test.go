package control

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
)

func TestNewScheduleRunCreatorWiresSessionStartCoordination(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	eventStream := &EventStream{log: slog.Default(), redis: redisClient}
	server, err := NewScheduleRunCreator(slog.Default(), fakeScheduleRunCreatorDB{}, nil, nil, eventStream)
	if err != nil {
		t.Fatal(err)
	}
	if server.eventStream != eventStream {
		t.Fatal("schedule run creator did not retain event stream for task-start coordination")
	}
}

func TestNewScheduleRunCreatorRequiresSessionStartCoordination(t *testing.T) {
	if _, err := NewScheduleRunCreator(slog.Default(), fakeScheduleRunCreatorDB{}, nil, nil, nil); err == nil {
		t.Fatal("expected missing event stream to be rejected")
	}
}

type fakeScheduleRunCreatorDB struct{}

func (fakeScheduleRunCreatorDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected exec")
}

func (fakeScheduleRunCreatorDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}

func (fakeScheduleRunCreatorDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeScheduleRunCreatorRow{}
}

func (fakeScheduleRunCreatorDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected begin")
}

type fakeScheduleRunCreatorRow struct{}

func (fakeScheduleRunCreatorRow) Scan(...any) error {
	return errors.New("unexpected scan")
}
