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
	"golang.org/x/sync/errgroup"
)

const (
	DefaultSweepEvery             = 5 * time.Second
	DefaultSweepLimit             = int32(100)
	DefaultMaterializeConcurrency = int32(10)
	DefaultFireConcurrency        = int32(10)
	DefaultFireLease              = 5 * time.Minute
	DefaultMaxAttempts            = int32(10)
	DefaultJitter                 = 30 * time.Second
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
	log                    *slog.Logger
	db                     *db.Queries
	tx                     txBeginner
	runner                 RunCreator
	interval               time.Duration
	limit                  int32
	materializeConcurrency int32
	fireConcurrency        int32
	lease                  time.Duration
	maxAttempts            int32
	jitter                 time.Duration
	now                    func() time.Time
}

type WorkerOption func(*Worker)

func WithSweepEvery(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.interval = value
	}
}

func WithSweepLimit(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.limit = value
	}
}

func WithMaterializeConcurrency(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.materializeConcurrency = value
	}
}

func WithFireConcurrency(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.fireConcurrency = value
	}
}

func WithLease(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.lease = value
	}
}

func WithMaxAttempts(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.maxAttempts = value
	}
}

func WithJitter(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.jitter = value
	}
}

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
		log:                    log,
		db:                     db.New(database),
		tx:                     database,
		runner:                 runner,
		interval:               DefaultSweepEvery,
		limit:                  DefaultSweepLimit,
		materializeConcurrency: DefaultMaterializeConcurrency,
		fireConcurrency:        DefaultFireConcurrency,
		lease:                  DefaultFireLease,
		maxAttempts:            DefaultMaxAttempts,
		jitter:                 DefaultJitter,
		now:                    func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(worker)
	}
	if worker.interval <= 0 || worker.limit <= 0 || worker.materializeConcurrency <= 0 || worker.fireConcurrency <= 0 || worker.lease <= 0 || worker.maxAttempts <= 0 || worker.now == nil {
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
	rows, err := w.db.WithTx(tx).ClaimDueScheduleInstances(ctx, db.ClaimDueScheduleInstancesParams{
		RowLimit:       w.limit,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTimeToPG(w.now().Add(w.lease)),
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(int(w.materializeConcurrency))
	for _, row := range rows {
		row := row
		group.Go(func() error {
			if err := w.materializeInstance(groupCtx, row); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				w.log.Error("schedule instance materialization failed", "schedule_id", ids.MustFromPG(row.ScheduleID).String(), "error", err)
			}
			return nil
		})
	}
	return group.Wait()
}

func (w *Worker) materializeInstance(ctx context.Context, row db.ClaimDueScheduleInstancesRow) error {
	if !row.NextScheduledAt.Valid {
		return nil
	}
	now := w.now()
	scheduledAt := row.NextScheduledAt.Time.UTC()
	snapshot, err := w.runner.SnapshotScheduleFire(ctx, row)
	if err != nil {
		w.log.Error("schedule fire snapshot failed", "schedule_id", ids.MustFromPG(row.ScheduleID).String(), "error", err)
		nextAttemptCount := row.MaterializeAttemptCount + 1
		_, err := w.db.MarkScheduleInstanceMaterializationFailed(ctx, db.MarkScheduleInstanceMaterializationFailedParams{
			ErrorMessage:       err.Error(),
			MaxAttempts:        w.maxAttempts,
			NextDueAt:          pgTimeToPG(now.Add(RetryDelay(nextAttemptCount))),
			InstanceID:         row.InstanceID,
			Generation:         row.Generation,
			MaterializeLeaseID: row.MaterializeLeaseID,
			NextScheduledAt:    pgTimeToPG(scheduledAt),
		})
		return err
	}
	tx, err := w.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := w.db.WithTx(tx)
	inserted, err := q.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
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
		MaterializeLeaseID: row.MaterializeLeaseID,
	})
	if err != nil {
		return err
	}
	advanced, err := w.advanceInstance(ctx, q, row, scheduledAt, now)
	if err != nil {
		return err
	}
	if inserted == 0 && advanced == 0 {
		return nil
	}
	return tx.Commit(ctx)
}

func (w *Worker) advanceInstance(ctx context.Context, q *db.Queries, row db.ClaimDueScheduleInstancesRow, scheduledAt time.Time, now time.Time) (int64, error) {
	anchor := scheduledAt
	if anchor.Before(now) {
		anchor = now
	}
	next, err := NextCronTime(row.Cron, row.Timezone, anchor)
	if err != nil {
		return 0, err
	}
	return q.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		NextScheduledAt:    pgTimeToPG(next),
		NextDueAt:          pgTimeToPG(next.Add(Jitter(ids.MustFromPG(row.InstanceID), w.jitter))),
		LastScheduledAt:    pgTimeToPG(scheduledAt),
		InstanceID:         row.InstanceID,
		Generation:         row.Generation,
		MaterializeLeaseID: row.MaterializeLeaseID,
	})
}

func (w *Worker) runFires(ctx context.Context) error {
	leaseID := ids.New()
	if _, err := w.db.DeleteExpiredScheduleFires(ctx, w.limit); err != nil {
		return err
	}
	if _, err := w.db.MarkExhaustedScheduleFires(ctx, db.MarkExhaustedScheduleFiresParams{
		MaxAttempts: w.maxAttempts,
		RowLimit:    w.limit,
	}); err != nil {
		return err
	}
	rows, err := w.db.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		LeaseID:        ids.ToPG(leaseID),
		LeaseExpiresAt: pgTimeToPG(w.now().Add(w.lease)),
		MaxAttempts:    w.maxAttempts,
		RowLimit:       w.limit,
	})
	if err != nil {
		return err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(int(w.fireConcurrency))
	for _, row := range rows {
		row := row
		group.Go(func() error {
			if err := w.runFire(groupCtx, leaseID, row); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				w.log.Error("schedule fire failed", "schedule_id", ids.MustFromPG(row.ScheduleID).String(), "error", err)
			}
			return nil
		})
	}
	return group.Wait()
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
		_, err := w.db.MarkScheduleFireSuperseded(ctx, db.MarkScheduleFireSupersededParams{
			ScheduleInstanceID: row.ScheduleInstanceID,
			ScheduledAt:        row.ScheduledAt,
			LeaseID:            ids.ToPG(leaseID),
		})
		return err
	}
	runID, err := w.runner.CreateScheduleRun(ctx, row, ids.ToPG(leaseID))
	if err != nil {
		if errors.Is(err, ErrFireSuperseded) {
			_, err := w.db.MarkScheduleFireSuperseded(ctx, db.MarkScheduleFireSupersededParams{
				ScheduleInstanceID: row.ScheduleInstanceID,
				ScheduledAt:        row.ScheduledAt,
				LeaseID:            ids.ToPG(leaseID),
			})
			return err
		}
		return w.markFireFailed(ctx, leaseID, row, err)
	}
	_, err = w.db.MarkScheduleFireCreated(ctx, db.MarkScheduleFireCreatedParams{
		RunID:              runID,
		ScheduleInstanceID: row.ScheduleInstanceID,
		ScheduledAt:        row.ScheduledAt,
		LeaseID:            ids.ToPG(leaseID),
	})
	return err
}

func (w *Worker) markFireFailed(ctx context.Context, leaseID uuid.UUID, row db.ClaimDueScheduleFiresRow, cause error) error {
	nextAttempt := w.now().Add(RetryDelay(row.AttemptCount))
	_, err := w.db.MarkScheduleFireFailed(ctx, db.MarkScheduleFireFailedParams{
		MaxAttempts:        w.maxAttempts,
		ErrorMessage:       cause.Error(),
		NextAttemptAt:      pgTimeToPG(nextAttempt),
		ScheduleInstanceID: row.ScheduleInstanceID,
		ScheduledAt:        row.ScheduledAt,
		LeaseID:            ids.ToPG(leaseID),
	})
	return err
}

func pgTimeToPG(value time.Time) pgtype.Timestamptz {
	if value.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}
