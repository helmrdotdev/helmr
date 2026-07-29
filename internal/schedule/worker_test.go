package schedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerClaimsAndAdmitsSchedule(t *testing.T) {
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	store := &workerStore{claimed: []db.Schedule{scheduleAt(now)}}
	admitter := &workerAdmitter{}
	worker, err := NewWorker(nil, store, admitter)
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }

	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(admitter.admissions) != 1 {
		t.Fatalf("admissions = %d, want 1", len(admitter.admissions))
	}
	if len(store.retryable) != 0 || len(store.errored) != 0 {
		t.Fatalf("unexpected transitions: retryable=%d errored=%d", len(store.retryable), len(store.errored))
	}
}

func TestWorkerBindsPendingWorkspaceBeforeClaiming(t *testing.T) {
	now := time.Date(2026, 6, 2, 0, 2, 0, 0, time.UTC)
	scheduleID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &workerStore{
		pending: []db.ListPendingScheduleBindingsRow{{
			ID:                  scheduleID,
			EnvironmentID:       environmentID,
			WorkspaceRefKey:     pgvalue.Text("scheduler"),
			CronPattern:         "*/5 * * * *",
			Timezone:            "UTC",
			Generation:          3,
			State:               "pending_workspace",
			ResolvedWorkspaceID: workspaceID,
		}},
	}
	worker, err := NewWorker(nil, store, &workerAdmitter{})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }

	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.activated) != 1 {
		t.Fatalf("activations = %d, want 1", len(store.activated))
	}
	got := store.activated[0]
	if got.ID != scheduleID ||
		got.EnvironmentID != environmentID ||
		got.WorkspaceID != workspaceID ||
		got.ExpectedGeneration != 3 ||
		!got.NextFireAt.Time.Equal(time.Date(2026, 6, 2, 0, 5, 0, 0, time.UTC)) {
		t.Fatalf("activation = %+v", got)
	}
}

func TestWorkerPersistsRetryStepAndSampledDelay(t *testing.T) {
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	value := scheduleAt(now)
	value.RetryStep = pgtype.Int2{Int16: 9, Valid: true}
	store := &workerStore{claimed: []db.Schedule{value}}
	admitter := &workerAdmitter{err: errors.New("database unavailable")}
	worker, err := NewWorker(nil, store, admitter)
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }
	worker.jitter = func(maximum time.Duration) (time.Duration, error) {
		if maximum != 5*time.Minute {
			t.Fatalf("jitter maximum = %s", maximum)
		}
		return 42 * time.Second, nil
	}

	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.retryable) != 1 {
		t.Fatalf("retry transitions = %d, want 1", len(store.retryable))
	}
	got := store.retryable[0]
	if got.RetryStep.Int16 != 10 || !got.RetryAfter.Time.Equal(now.Add(42*time.Second)) {
		t.Fatalf("retry transition = %+v", got)
	}
}

func TestWorkerPersistsBoundedPermanentError(t *testing.T) {
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	store := &workerStore{claimed: []db.Schedule{scheduleAt(now)}}
	admitter := &workerAdmitter{err: &AdmissionError{
		Code:    ErrorWorkspaceUnavailable,
		Message: string(make([]byte, 2048)),
	}}
	worker, err := NewWorker(nil, store, admitter)
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }

	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.errored) != 1 {
		t.Fatalf("error transitions = %d, want 1", len(store.errored))
	}
	lastError := store.errored[0]
	if lastError.LastErrorCode.String != string(ErrorWorkspaceUnavailable) {
		t.Fatalf("last error code = %q", lastError.LastErrorCode.String)
	}
	if len(lastError.LastErrorMessage.String) > 1024 {
		t.Fatalf("last error was not bounded: %d bytes", len(lastError.LastErrorMessage.String))
	}
}

func TestTruncateUTF8SuppliesValidNonemptyDiagnostic(t *testing.T) {
	for _, value := range []string{"", " \t", "failure\xffdetail", strings.Repeat(" ", 1024) + "detail"} {
		got := truncateUTF8(value, 1024)
		if got == "" {
			t.Fatalf("truncateUTF8(%q) returned empty", value)
		}
		if len(got) > 1024 {
			t.Fatalf("truncateUTF8(%q) returned %d bytes", value, len(got))
		}
	}
}

func scheduleAt(at time.Time) db.Schedule {
	return db.Schedule{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CronPattern:          "0 9 * * *",
		Timezone:             "Asia/Tokyo",
		CronSemanticsVersion: CronSemanticsVersion,
		Generation:           3,
		NextFireAt:           pgvalue.TimestamptzUTCZeroInvalid(at),
		ClaimedBy:            pgvalue.Text("worker"),
	}
}

type workerStore struct {
	pending   []db.ListPendingScheduleBindingsRow
	activated []db.ActivatePendingScheduleParams
	claimed   []db.Schedule
	retryable []db.MarkScheduleAdmissionRetryableParams
	errored   []db.MarkScheduleAdmissionErroredParams
}

func (s *workerStore) ListPendingScheduleBindings(context.Context, int32) ([]db.ListPendingScheduleBindingsRow, error) {
	return s.pending, nil
}

func (s *workerStore) ActivatePendingSchedule(_ context.Context, value db.ActivatePendingScheduleParams) (db.Schedule, error) {
	s.activated = append(s.activated, value)
	return db.Schedule{}, nil
}

func (s *workerStore) ClaimDueSchedules(context.Context, db.ClaimDueSchedulesParams) ([]db.Schedule, error) {
	return s.claimed, nil
}

func (s *workerStore) MarkScheduleAdmissionRetryable(_ context.Context, value db.MarkScheduleAdmissionRetryableParams) (db.Schedule, error) {
	s.retryable = append(s.retryable, value)
	return db.Schedule{}, nil
}

func (s *workerStore) MarkScheduleAdmissionErrored(_ context.Context, value db.MarkScheduleAdmissionErroredParams) (db.Schedule, error) {
	s.errored = append(s.errored, value)
	return db.Schedule{}, nil
}

type workerAdmitter struct {
	admissions []db.Schedule
	err        error
}

func (a *workerAdmitter) AdmitSchedule(_ context.Context, value db.Schedule) error {
	a.admissions = append(a.admissions, value)
	return a.err
}
