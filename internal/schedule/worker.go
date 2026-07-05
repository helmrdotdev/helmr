package schedule

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"
)

const (
	DefaultRepairEvery        = 5 * time.Second
	DefaultRepairLimit        = int32(100)
	DefaultTriggerConcurrency = int32(10)
	DefaultTriggerLease       = 5 * time.Minute
	DefaultMaxAttempts        = int32(10)
	DefaultJitter             = 30 * time.Second
	DefaultRepairLookahead    = 2*DefaultRepairEvery + DefaultJitter
	reconcileUnlockTimeout    = 5 * time.Second
)

type RunCreator interface {
	CreateScheduleRun(context.Context, db.GetScheduleTriggerCandidateRow) (pgtype.UUID, error)
}

type Index interface {
	Enqueue(context.Context, IndexEntry) error
	Delete(context.Context, string, uuid.UUID) error
	Dequeue(context.Context, DequeueRequest) ([]IndexLease, error)
	Ack(context.Context, IndexLease) error
	Nack(context.Context, IndexLease, time.Time) error
}

type dbConn interface {
	db.DBTX
}

type RepairStore interface {
	ListScheduleRepairEntries(context.Context, db.ListScheduleRepairEntriesParams) ([]db.ListScheduleRepairEntriesRow, error)
}

type ReconcileLock interface {
	TryLock(ctx context.Context) (ReconcileLockGuard, bool, error)
}

type ReconcileLockGuard interface {
	Store(fallback RepairStore) RepairStore
	Unlock(ctx context.Context) error
}

type Worker struct {
	log         *slog.Logger
	engine      *Engine
	workerID    uuid.UUID
	interval    time.Duration
	limit       int32
	concurrency int32
	lease       time.Duration
	now         func() time.Time
}

type WorkerOption func(*Worker)

func WithRepairEvery(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.interval = value
	}
}

func WithRepairLimit(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.limit = value
	}
}

func WithTriggerConcurrency(value int32) WorkerOption {
	return func(worker *Worker) {
		worker.concurrency = value
	}
}

func WithLease(value time.Duration) WorkerOption {
	return func(worker *Worker) {
		worker.lease = value
	}
}

func NewWorker(log *slog.Logger, engine *Engine, opts ...WorkerOption) (*Worker, error) {
	if log == nil {
		log = slog.Default()
	}
	if engine == nil {
		return nil, errors.New("schedule engine is required")
	}
	worker := &Worker{
		log:         log,
		engine:      engine,
		workerID:    uuid.Must(uuid.NewV7()),
		interval:    DefaultRepairEvery,
		limit:       DefaultRepairLimit,
		concurrency: DefaultTriggerConcurrency,
		lease:       DefaultTriggerLease,
		now:         func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(worker)
	}
	if worker.interval <= 0 || worker.limit <= 0 || worker.concurrency <= 0 || worker.lease <= 0 || worker.now == nil {
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
	if err := w.engine.Repair(ctx); err != nil {
		return err
	}
	return w.runDue(ctx)
}

func (w *Worker) runDue(ctx context.Context) error {
	leases, err := w.engine.index.Dequeue(ctx, DequeueRequest{
		CellID:   w.engine.cellID,
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
		group.Go(func() error {
			if err := w.engine.Fire(groupCtx, lease); err != nil {
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

func triggerErrorKind(err error) string {
	if err == nil {
		return ""
	}
	return "trigger_failed"
}
