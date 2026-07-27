package schedule

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"
)

const (
	DefaultPollInterval = time.Second
	DefaultClaimLimit   = int32(100)
	DefaultConcurrency  = int32(10)
	DefaultClaimLease   = 5 * time.Minute
)

var ErrClaimSuperseded = errors.New("schedule claim was superseded")

type ErrorCode string

const (
	ErrorTaskAuthorityInvalid     ErrorCode = "task-authority-invalid"
	ErrorWorkspaceUnavailable     ErrorCode = "workspace-unavailable"
	ErrorArchitectureIncompatible ErrorCode = "architecture-incompatible"
	ErrorGenerationInvalid        ErrorCode = "generation-invalid"
	ErrorInputInvalid             ErrorCode = "input-invalid"
)

type AdmissionError struct {
	Code    ErrorCode
	Message string
}

func (e *AdmissionError) Error() string {
	return e.Message
}

type Store interface {
	ActivatePendingSchedule(context.Context, db.ActivatePendingScheduleParams) (db.Schedule, error)
	ClaimDueSchedules(context.Context, db.ClaimDueSchedulesParams) ([]db.Schedule, error)
	ListPendingScheduleBindings(context.Context, int32) ([]db.ListPendingScheduleBindingsRow, error)
	MarkScheduleAdmissionErrored(context.Context, db.MarkScheduleAdmissionErroredParams) (db.Schedule, error)
	MarkScheduleAdmissionRetryable(context.Context, db.MarkScheduleAdmissionRetryableParams) (db.Schedule, error)
}

type Admitter interface {
	AdmitSchedule(context.Context, db.Schedule) error
}

type Worker struct {
	log         *slog.Logger
	store       Store
	admitter    Admitter
	workerID    string
	interval    time.Duration
	limit       int32
	concurrency int32
	lease       time.Duration
	now         func() time.Time
	jitter      func(time.Duration) (time.Duration, error)
}

type WorkerOption func(*Worker)

func WithPollInterval(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.interval = value
	}
}

func WithClaimLimit(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.limit = value
	}
}

func WithConcurrency(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.concurrency = value
	}
}

func WithClaimLease(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.lease = value
	}
}

func NewWorker(log *slog.Logger, store Store, admitter Admitter, opts ...WorkerOption) (*Worker, error) {
	if log == nil {
		log = slog.Default()
	}
	if store == nil {
		return nil, errors.New("schedule store is required")
	}
	if admitter == nil {
		return nil, errors.New("schedule admitter is required")
	}
	worker := &Worker{
		log:         log,
		store:       store,
		admitter:    admitter,
		workerID:    uuid.Must(uuid.NewV7()).String(),
		interval:    DefaultPollInterval,
		limit:       DefaultClaimLimit,
		concurrency: DefaultConcurrency,
		lease:       DefaultClaimLease,
		now:         func() time.Time { return time.Now().UTC() },
		jitter:      randomJitter,
	}
	for _, option := range opts {
		option(worker)
	}
	if worker.interval <= 0 || worker.limit <= 0 || worker.concurrency <= 0 || worker.lease <= 0 {
		return nil, errors.New("invalid schedule worker configuration")
	}
	return worker, nil
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("schedule worker tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) tick(ctx context.Context) error {
	now := w.now().UTC()
	if err := w.bindPending(ctx, now); err != nil {
		return err
	}
	claimed, err := w.store.ClaimDueSchedules(ctx, db.ClaimDueSchedulesParams{
		ClaimedBy:      pgtype.Text{String: w.workerID, Valid: true},
		ClaimExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(now.Add(w.lease)),
		LimitCount:     w.limit,
	})
	if err != nil {
		return err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(int(w.concurrency))
	for _, value := range claimed {
		value := value
		group.Go(func() error {
			if err := w.process(groupCtx, value); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				w.log.Error("schedule admission failed", "schedule_id", value.PublicID, "error", err)
			}
			return nil
		})
	}
	return group.Wait()
}

func (w *Worker) bindPending(ctx context.Context, now time.Time) error {
	pending, err := w.store.ListPendingScheduleBindings(ctx, w.limit)
	if err != nil {
		return err
	}
	for _, candidate := range pending {
		next, err := NextCronTime(candidate.CronPattern, candidate.Timezone, now)
		if err != nil {
			return fmt.Errorf("calculate pending Schedule %s cursor: %w", candidate.PublicID, err)
		}
		_, err = w.store.ActivatePendingSchedule(ctx, db.ActivatePendingScheduleParams{
			WorkspaceID:        candidate.ResolvedWorkspaceID,
			EffectiveFrom:      pgvalue.Timestamptz(now),
			NextFireAt:         pgvalue.Timestamptz(next),
			EnvironmentID:      candidate.EnvironmentID,
			ID:                 candidate.ID,
			ExpectedGeneration: candidate.Generation,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) process(ctx context.Context, value db.Schedule) error {
	err := w.admitter.AdmitSchedule(ctx, value)
	if err == nil || errors.Is(err, ErrClaimSuperseded) {
		return nil
	}
	var permanent *AdmissionError
	if errors.As(err, &permanent) {
		return w.markErrored(ctx, value, permanent)
	}
	if markErr := w.markRetryable(ctx, value); markErr != nil {
		return errors.Join(err, markErr)
	}
	return err
}

func (w *Worker) markErrored(ctx context.Context, value db.Schedule, admissionErr *AdmissionError) error {
	if !validErrorCode(admissionErr.Code) {
		return fmt.Errorf("invalid Schedule error code %q", admissionErr.Code)
	}
	_, err := w.store.MarkScheduleAdmissionErrored(ctx, db.MarkScheduleAdmissionErroredParams{
		LastErrorCode:       pgvalue.Text(string(admissionErr.Code)),
		LastErrorMessage:    pgvalue.Text(truncateUTF8(admissionErr.Message, 1024)),
		EnvironmentID:       value.EnvironmentID,
		ID:                  value.ID,
		ExpectedGeneration:  value.Generation,
		ExpectedScheduledAt: value.NextFireAt,
		ClaimedBy:           value.ClaimedBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (w *Worker) markRetryable(ctx context.Context, value db.Schedule) error {
	step := int16(1)
	if value.RetryStep.Valid {
		step = min(int16(10), value.RetryStep.Int16+1)
	}
	delay, err := w.jitter(retryBase(step))
	if err != nil {
		return err
	}
	_, err = w.store.MarkScheduleAdmissionRetryable(ctx, db.MarkScheduleAdmissionRetryableParams{
		RetryStep:           pgtype.Int2{Int16: step, Valid: true},
		RetryAfter:          pgvalue.TimestamptzUTCZeroInvalid(w.now().UTC().Add(delay)),
		EnvironmentID:       value.EnvironmentID,
		ID:                  value.ID,
		ExpectedGeneration:  value.Generation,
		ExpectedScheduledAt: value.NextFireAt,
		ExpectedRetryStep:   value.RetryStep,
		ClaimedBy:           value.ClaimedBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func retryBase(step int16) time.Duration {
	if step < 1 {
		step = 1
	}
	if step > 10 {
		step = 10
	}
	base := time.Second << (step - 1)
	return min(5*time.Minute, base)
}

func randomJitter(maximum time.Duration) (time.Duration, error) {
	if maximum <= 0 {
		return 0, nil
	}
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return 0, fmt.Errorf("sample schedule retry jitter: %w", err)
	}
	return time.Duration(binary.BigEndian.Uint64(buffer[:]) % (uint64(maximum) + 1)), nil
}

func validErrorCode(code ErrorCode) bool {
	switch code {
	case ErrorTaskAuthorityInvalid,
		ErrorWorkspaceUnavailable,
		ErrorArchitectureIncompatible,
		ErrorGenerationInvalid,
		ErrorInputInvalid:
		return true
	default:
		return false
	}
}

func truncateUTF8(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	value = strings.ToValidUTF8(value, "")
	if strings.TrimSpace(value) == "" {
		return "Schedule admission failed"
	}
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	if strings.TrimSpace(value) == "" {
		return "Schedule admission failed"
	}
	return value
}
