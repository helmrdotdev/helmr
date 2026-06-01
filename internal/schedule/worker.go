package schedule

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DefaultSweepEvery = 5 * time.Second
	DefaultSweepLimit = int32(100)
	DefaultFireLease  = 5 * time.Minute
	DefaultJitter     = 30 * time.Second
)

type RunCreator interface {
	SnapshotScheduleFire(context.Context, db.ClaimDueScheduleInstancesRow) (FireSnapshot, error)
	CreateScheduleRun(context.Context, db.ClaimDueScheduleFiresRow, pgtype.UUID) (pgtype.UUID, error)
}

type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type dbTXBeginner interface {
	db.DBTX
	txBeginner
}

type Worker struct {
	log      *slog.Logger
	db       *db.Queries
	tx       txBeginner
	runner   RunCreator
	interval time.Duration
	limit    int32
	lease    time.Duration
	jitter   time.Duration
	now      func() time.Time
}

type WorkerOption func(*Worker)

func NewWorker(log *slog.Logger, database dbTXBeginner, runner RunCreator, opts ...WorkerOption) (*Worker, error) {
	if log == nil {
		log = slog.Default()
	}
	if database == nil {
		return nil, errors.New("database is required")
	}
	if runner == nil {
		return nil, errors.New("run creator is required")
	}
	worker := &Worker{
		log:      log,
		db:       db.New(database),
		tx:       database,
		runner:   runner,
		interval: DefaultSweepEvery,
		limit:    DefaultSweepLimit,
		lease:    DefaultFireLease,
		jitter:   DefaultJitter,
		now:      func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(worker)
	}
	if worker.interval <= 0 || worker.limit <= 0 || worker.lease <= 0 || worker.now == nil {
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
	if err := w.materialize(ctx); err != nil {
		return err
	}
	return w.runFires(ctx)
}

func (w *Worker) materialize(ctx context.Context) error {
	tx, err := w.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := w.db.WithTx(tx)
	rows, err := q.ClaimDueScheduleInstances(ctx, w.limit)
	if err != nil {
		return err
	}
	now := w.now()
	for _, row := range rows {
		if !row.NextScheduledAt.Valid {
			continue
		}
		scheduledAt := row.NextScheduledAt.Time.UTC()
		snapshot, err := w.runner.SnapshotScheduleFire(ctx, row)
		if err != nil {
			w.log.Error("schedule fire snapshot failed", "schedule_id", ids.MustFromPG(row.ScheduleID).String(), "error", err)
			if err := w.advanceInstance(ctx, q, row, scheduledAt, now); err != nil {
				return err
			}
			continue
		}
		if _, err := q.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
			ScheduleInstanceID: row.InstanceID,
			ScheduledAt:        pgTimeToPG(scheduledAt),
			ScheduleID:         row.ScheduleID,
			OrgID:              row.OrgID,
			ProjectID:          row.ProjectID,
			EnvironmentID:      row.EnvironmentID,
			Generation:         row.Generation,
			TaskID:             snapshot.TaskID,
			Payload:            snapshot.Payload,
			SecretBindings:     snapshot.SecretBindings,
			Workspace:          snapshot.Workspace,
			RunOptions:         snapshot.RunOptions,
		}); err != nil {
			return err
		}
		if err := w.advanceInstance(ctx, q, row, scheduledAt, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (w *Worker) advanceInstance(ctx context.Context, q *db.Queries, row db.ClaimDueScheduleInstancesRow, scheduledAt time.Time, now time.Time) error {
	anchor := scheduledAt
	if anchor.Before(now) {
		anchor = now
	}
	next, err := NextCronTime(row.CronExpression, row.Timezone, anchor)
	if err != nil {
		return err
	}
	return q.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		NextScheduledAt: pgTimeToPG(next),
		NextDueAt:       pgTimeToPG(next.Add(Jitter(ids.MustFromPG(row.InstanceID), w.jitter))),
		LastScheduledAt: pgTimeToPG(scheduledAt),
		InstanceID:      row.InstanceID,
		Generation:      row.Generation,
	})
}

func (w *Worker) runFires(ctx context.Context) error {
	leaseID := ids.New()
	rows, err := w.db.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		LeaseID:        ids.ToPG(leaseID),
		LeaseExpiresAt: pgTimeToPG(w.now().Add(w.lease)),
		RowLimit:       w.limit,
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.runFire(ctx, leaseID, row); err != nil {
			w.log.Error("schedule fire failed", "schedule_id", ids.MustFromPG(row.ScheduleID).String(), "error", err)
		}
	}
	return nil
}

func (w *Worker) runFire(ctx context.Context, leaseID uuid.UUID, row db.ClaimDueScheduleFiresRow) error {
	current, err := w.db.ScheduleFireLeaseIsCurrent(ctx, db.ScheduleFireLeaseIsCurrentParams{
		ScheduleInstanceID: row.ScheduleInstanceID,
		ScheduledAt:        row.ScheduledAt,
		LeaseID:            ids.ToPG(leaseID),
	})
	if err != nil {
		return err
	}
	if !current {
		return w.db.MarkScheduleFireSuperseded(ctx, db.MarkScheduleFireSupersededParams{
			ScheduleInstanceID: row.ScheduleInstanceID,
			ScheduledAt:        row.ScheduledAt,
			LeaseID:            ids.ToPG(leaseID),
		})
	}
	runID, err := w.runner.CreateScheduleRun(ctx, row, ids.ToPG(leaseID))
	if err != nil {
		if errors.Is(err, ErrFireSuperseded) {
			return w.db.MarkScheduleFireSuperseded(ctx, db.MarkScheduleFireSupersededParams{
				ScheduleInstanceID: row.ScheduleInstanceID,
				ScheduledAt:        row.ScheduledAt,
				LeaseID:            ids.ToPG(leaseID),
			})
		}
		return w.markFireFailed(ctx, leaseID, row, err)
	}
	return w.db.MarkScheduleFireCreated(ctx, db.MarkScheduleFireCreatedParams{
		RunID:              runID,
		ScheduleInstanceID: row.ScheduleInstanceID,
		ScheduledAt:        row.ScheduledAt,
		LeaseID:            ids.ToPG(leaseID),
	})
}

func (w *Worker) markFireFailed(ctx context.Context, leaseID uuid.UUID, row db.ClaimDueScheduleFiresRow, cause error) error {
	nextAttempt := w.now().Add(RetryDelay(row.AttemptCount))
	return w.db.MarkScheduleFireFailed(ctx, db.MarkScheduleFireFailedParams{
		ErrorMessage:       cause.Error(),
		NextAttemptAt:      pgTimeToPG(nextAttempt),
		ScheduleInstanceID: row.ScheduleInstanceID,
		ScheduledAt:        row.ScheduledAt,
		LeaseID:            ids.ToPG(leaseID),
	})
}

func pgTimeToPG(value time.Time) pgtype.Timestamptz {
	if value.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}
