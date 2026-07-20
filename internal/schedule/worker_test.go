package schedule

import (
	"context"
	"encoding/json"
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
	var lastError struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(store.errored[0].LastError, &lastError); err != nil {
		t.Fatal(err)
	}
	if len(lastError.Message) > 1024 {
		t.Fatalf("last error was not bounded: %d bytes", len(lastError.Message))
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
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:            "sch_abcdefghijklmnopqrstuvwxyz",
		EnvironmentID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Source:              "imperative",
		Key:                 "daily-report",
		CronPattern:         "0 9 * * *",
		Timezone:            "Asia/Tokyo",
		CronContractVersion: CronContractVersion,
		Generation:          3,
		NextFireAt:          pgvalue.TimestamptzUTCZeroInvalid(at),
		ClaimedBy:           pgvalue.Text("worker"),
	}
}

type workerStore struct {
	claimed   []db.Schedule
	retryable []db.MarkScheduleAdmissionRetryableParams
	errored   []db.MarkScheduleAdmissionErroredParams
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
	admissions []Admission
	err        error
}

func (a *workerAdmitter) AdmitSchedule(_ context.Context, value Admission) error {
	a.admissions = append(a.admissions, value)
	return a.err
}
