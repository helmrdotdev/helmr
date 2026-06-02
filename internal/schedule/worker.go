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
	DefaultSweepEvery         = 5 * time.Second
	DefaultSweepLimit         = int32(100)
	DefaultTriggerConcurrency = int32(10)
	DefaultTriggerLease       = 5 * time.Minute
	DefaultMaxAttempts        = int32(10)
	DefaultJitter             = 30 * time.Second
	DefaultIndexLookahead     = 2*DefaultSweepEvery + DefaultJitter
	reconcileUnlockTimeout    = 5 * time.Second
)

type RunCreator interface {
	CreateScheduleRun(context.Context, db.GetScheduleTriggerCandidateRow) (pgtype.UUID, error)
}

type Index interface {
	Enqueue(context.Context, IndexEntry) error
	Dequeue(context.Context, DequeueRequest) ([]IndexLease, error)
	Ack(context.Context, IndexLease) error
	Nack(context.Context, IndexLease, time.Time) error
}

type dbConn interface {
	db.DBTX
}

type ReconcileStore interface {
	ListScheduleIndexEntries(context.Context, db.ListScheduleIndexEntriesParams) ([]db.ListScheduleIndexEntriesRow, error)
}

type ReconcileLock interface {
	TryLock(ctx context.Context) (ReconcileLockGuard, bool, error)
}

type ReconcileLockGuard interface {
	Store(fallback ReconcileStore) ReconcileStore
	Unlock(ctx context.Context) error
}

type Worker struct {
	log         *slog.Logger
	db          *db.Queries
	lock        ReconcileLock
	index       Index
	runner      RunCreator
	workerID    uuid.UUID
	interval    time.Duration
	limit       int32
	concurrency int32
	lookahead   time.Duration
	lease       time.Duration
	maxAttempts int32
	jitter      time.Duration
	now         func() time.Time
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

func WithTriggerConcurrency(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.concurrency = value
	}
}

func WithIndexLookahead(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.lookahead = value
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

func WithReconcileLock(lock ReconcileLock) WorkerOption {
	return func(worker *Worker) {
		worker.lock = lock
	}
}

func NewWorker(log *slog.Logger, database dbConn, index Index, runner RunCreator, opts ...WorkerOption) (*Worker, error) {
	if log == nil {
		log = slog.Default()
	}
	if database == nil {
		return nil, errors.New("database is required")
	}
	if index == nil {
		return nil, errors.New("schedule index is required")
	}
	if runner == nil {
		return nil, errors.New("run creator is required")
	}
	worker := &Worker{
		log:         log,
		db:          db.New(database),
		index:       index,
		runner:      runner,
		workerID:    ids.New(),
		interval:    DefaultSweepEvery,
		limit:       DefaultSweepLimit,
		concurrency: DefaultTriggerConcurrency,
		lookahead:   DefaultIndexLookahead,
		lease:       DefaultTriggerLease,
		maxAttempts: DefaultMaxAttempts,
		jitter:      DefaultJitter,
		now:         func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(worker)
	}
	if worker.interval <= 0 || worker.limit <= 0 || worker.concurrency <= 0 || worker.lookahead <= 0 || worker.lease <= 0 || worker.maxAttempts <= 0 || worker.now == nil {
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
	if err := w.reconcileIndex(ctx); err != nil {
		return err
	}
	return w.runDue(ctx)
}

func (w *Worker) reconcileIndex(ctx context.Context) error {
	store := ReconcileStore(w.db)
	var guard ReconcileLockGuard
	if w.lock != nil {
		var locked bool
		var err error
		guard, locked, err = w.lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !locked {
			w.log.Debug("schedule reconcile lock is held by another instance")
			return nil
		}
		store = guard.Store(store)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				w.log.Warn("release schedule reconcile lock failed", "error", err)
			}
		}()
	}
	availableBefore := pgTimeToPG(w.now().Add(w.lookahead))
	var afterAvailableAt pgtype.Timestamptz
	var afterInstanceID pgtype.UUID
	for {
		rows, err := store.ListScheduleIndexEntries(ctx, db.ListScheduleIndexEntriesParams{
			AvailableBefore:  availableBefore,
			AfterAvailableAt: afterAvailableAt,
			AfterInstanceID:  afterInstanceID,
			RowLimit:         w.limit,
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !row.NextScheduledAt.Valid {
				continue
			}
			if err := w.index.Enqueue(ctx, IndexEntry{
				InstanceID:  ids.MustFromPG(row.InstanceID),
				Generation:  row.Generation,
				ScheduledAt: row.NextScheduledAt.Time.UTC(),
				AvailableAt: w.availableAt(row.InstanceID, row.NextScheduledAt, row.RetryAfter),
			}); err != nil {
				return err
			}
		}
		if len(rows) < int(w.limit) {
			break
		}
		last := rows[len(rows)-1]
		afterAvailableAt = last.AvailableAt
		afterInstanceID = last.InstanceID
	}
	return nil
}

func (w *Worker) runDue(ctx context.Context) error {
	leases, err := w.index.Dequeue(ctx, DequeueRequest{
		WorkerID: w.workerID,
		Limit:    w.limit,
		Now:      w.now(),
		Lease:    w.lease,
	})
	if err != nil {
		return err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(int(w.concurrency))
	for _, lease := range leases {
		lease := lease
		group.Go(func() error {
			if err := w.runLease(groupCtx, lease); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				w.log.Error("schedule trigger failed", "instance_id", lease.Entry.InstanceID.String(), "error", err)
			}
			return nil
		})
	}
	return group.Wait()
}

func (w *Worker) runLease(ctx context.Context, lease IndexLease) error {
	candidate, err := w.db.GetScheduleTriggerCandidate(ctx, db.GetScheduleTriggerCandidateParams{
		InstanceID:  ids.ToPG(lease.Entry.InstanceID),
		Generation:  lease.Entry.Generation,
		ScheduledAt: pgTimeToPG(lease.Entry.ScheduledAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		retryAfter, retryErr := w.db.GetScheduleRetryAfter(ctx, db.GetScheduleRetryAfterParams{
			InstanceID:  ids.ToPG(lease.Entry.InstanceID),
			Generation:  lease.Entry.Generation,
			ScheduledAt: pgTimeToPG(lease.Entry.ScheduledAt),
		})
		if retryErr == nil && retryAfter.Valid {
			return w.index.Nack(ctx, lease, retryAfter.Time.UTC())
		}
		if retryErr != nil && !errors.Is(retryErr, pgx.ErrNoRows) {
			return retryErr
		}
		return w.index.Ack(ctx, lease)
	}
	if err != nil {
		return err
	}
	runID, err := w.runner.CreateScheduleRun(ctx, candidate)
	if err != nil {
		if errors.Is(err, ErrTriggerSuperseded) {
			return w.index.Ack(ctx, lease)
		}
		return w.markTriggerFailed(ctx, lease, candidate, err)
	}
	if !runID.Valid {
		return errors.New("created schedule run has no id")
	}
	next, err := w.nextScheduledAt(candidate, w.now())
	if err != nil {
		return w.markTriggerFailed(ctx, lease, candidate, err)
	}
	advanced, err := w.db.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		NextScheduledAt: pgTimeToPG(next),
		LastScheduledAt: candidate.NextScheduledAt,
		InstanceID:      candidate.InstanceID,
		Generation:      candidate.Generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return w.index.Ack(ctx, lease)
	}
	if err != nil {
		return err
	}
	if advanced.NextScheduledAt.Valid {
		if err := w.index.Enqueue(ctx, IndexEntry{
			InstanceID:  ids.MustFromPG(advanced.InstanceID),
			Generation:  advanced.Generation,
			ScheduledAt: advanced.NextScheduledAt.Time.UTC(),
			AvailableAt: advanced.NextScheduledAt.Time.UTC().Add(Jitter(ids.MustFromPG(advanced.InstanceID), w.jitter)),
		}); err != nil {
			return err
		}
	}
	return w.index.Ack(ctx, lease)
}

func (w *Worker) markTriggerFailed(ctx context.Context, lease IndexLease, row db.GetScheduleTriggerCandidateRow, cause error) error {
	nextAttempt := row.TriggerAttemptCount + 1
	retryAt := w.now().Add(RetryDelay(nextAttempt))
	_, err := w.db.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		ErrorMessage: cause.Error(),
		RetryAfter:   pgTimeToPG(retryAt),
		InstanceID:   row.InstanceID,
		Generation:   row.Generation,
		ScheduledAt:  row.NextScheduledAt,
	})
	if err != nil {
		return err
	}
	if nextAttempt >= w.maxAttempts {
		return w.skipFailedSlot(ctx, lease, row)
	}
	if nackErr := w.index.Nack(ctx, lease, retryAt); nackErr != nil {
		return nackErr
	}
	return cause
}

func (w *Worker) skipFailedSlot(ctx context.Context, lease IndexLease, row db.GetScheduleTriggerCandidateRow) error {
	next, err := w.nextScheduledAt(row, w.now())
	if err != nil {
		return err
	}
	skipped, err := w.db.SkipScheduleInstanceTrigger(ctx, db.SkipScheduleInstanceTriggerParams{
		NextScheduledAt: pgTimeToPG(next),
		LastScheduledAt: row.NextScheduledAt,
		InstanceID:      row.InstanceID,
		Generation:      row.Generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return w.index.Ack(ctx, lease)
	}
	if err != nil {
		return err
	}
	if skipped.NextScheduledAt.Valid {
		if err := w.index.Enqueue(ctx, IndexEntry{
			InstanceID:  ids.MustFromPG(skipped.InstanceID),
			Generation:  skipped.Generation,
			ScheduledAt: skipped.NextScheduledAt.Time.UTC(),
			AvailableAt: skipped.NextScheduledAt.Time.UTC().Add(Jitter(ids.MustFromPG(skipped.InstanceID), w.jitter)),
		}); err != nil {
			return err
		}
	}
	return w.index.Ack(ctx, lease)
}

func (w *Worker) nextScheduledAt(row db.GetScheduleTriggerCandidateRow, now time.Time) (time.Time, error) {
	anchor := row.NextScheduledAt.Time.UTC()
	if anchor.Before(now) {
		anchor = now
	}
	return NextCronTime(row.Cron, row.Timezone, anchor)
}

func (w *Worker) availableAt(instanceID pgtype.UUID, scheduledAt pgtype.Timestamptz, retryAfter pgtype.Timestamptz) time.Time {
	if retryAfter.Valid {
		return retryAfter.Time.UTC()
	}
	return scheduledAt.Time.UTC().Add(Jitter(ids.MustFromPG(instanceID), w.jitter))
}

func pgTimeToPG(value time.Time) pgtype.Timestamptz {
	if value.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}
