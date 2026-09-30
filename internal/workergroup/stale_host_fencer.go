package workergroup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultWorkerRegistrationReadinessGrace = 15 * time.Minute
	DefaultStaleHostFenceBatch              = int32(100)
	DefaultStaleHostFenceEvery              = 5 * time.Second
	DefaultStaleHostFenceTimeout            = 30 * time.Second
	DefaultStaleHostFenceBackoff            = 30 * time.Second

	// staleHostFenceLockName is the persisted advisory lock identity shared by
	// every dispatcher replica; changing it would let two replicas fence at once.
	staleHostFenceLockName = "helmr.dispatcher.stale_worker_fencer"
	staleHostReasonCode    = "worker_observation_stale"
)

// staleHostFenceQueries selects candidates while locking their worker host rows.
// The transition rechecks freshness defensively under the retained lock.
type staleHostFenceQueries interface {
	ListStaleWorkerFenceCandidates(context.Context, db.ListStaleWorkerFenceCandidatesParams) ([]db.ListStaleWorkerFenceCandidatesRow, error)
	RecheckAndFenceStaleWorkerHost(context.Context, db.RecheckAndFenceStaleWorkerHostParams) (db.RecheckAndFenceStaleWorkerHostRow, error)
}

type staleHostFenceTransactions interface {
	withinStaleHostFenceTransaction(context.Context, func(staleHostFenceQueries) error) error
}

type StaleHostFenceClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type StaleHostFenceOutcome string

const (
	StaleHostFenced  StaleHostFenceOutcome = "fenced"
	StaleHostSkipped StaleHostFenceOutcome = "skipped"
)

type StaleHostFenceResult struct {
	WorkerHostID   pgtype.UUID
	WorkerGroupID  pgtype.UUID
	WorkerEpoch    pgtype.Int8
	PreviousStatus db.WorkerHostStatus
	FreshnessAt    time.Time
	Outcome        StaleHostFenceOutcome
	Reason         string
}

type StaleHostFenceCycle struct {
	LockAcquired bool
	Selected     int
	Fenced       int
	Skipped      int
	Results      []StaleHostFenceResult
}

// StaleHostFencer fences worker hosts whose observations went stale. One
// dispatcher replica at a time runs a cycle, serialized by a session-level
// advisory lock; the cycle's READ COMMITTED transaction runs on the connection
// that holds the lock.
type StaleHostFencer struct {
	transactions staleHostFenceTransactions
	lockPool     *pgxpool.Pool
	every        time.Duration
	timeout      time.Duration
	maxBackoff   time.Duration
	log          *slog.Logger
	clock        StaleHostFenceClock
}

type StaleHostFencerOption func(*StaleHostFencer)

func WithStaleHostFenceInterval(every time.Duration) StaleHostFencerOption {
	return func(fencer *StaleHostFencer) { fencer.every = every }
}

func WithStaleHostFenceTimeout(timeout time.Duration) StaleHostFencerOption {
	return func(fencer *StaleHostFencer) { fencer.timeout = timeout }
}

func WithStaleHostFenceMaxBackoff(maxBackoff time.Duration) StaleHostFencerOption {
	return func(fencer *StaleHostFencer) { fencer.maxBackoff = maxBackoff }
}

func WithStaleHostFenceLogger(log *slog.Logger) StaleHostFencerOption {
	return func(fencer *StaleHostFencer) { fencer.log = log }
}

func WithStaleHostFenceClock(clock StaleHostFenceClock) StaleHostFencerOption {
	return func(fencer *StaleHostFencer) { fencer.clock = clock }
}

func NewStaleHostFencer(pool *pgxpool.Pool, opts ...StaleHostFencerOption) (*StaleHostFencer, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	return newStaleHostFencer(pgxStaleHostFenceTransactions{beginner: pool}, pool, opts...)
}

// newStaleHostFencer runs cycles without the advisory lock when lockPool is nil.
func newStaleHostFencer(transactions staleHostFenceTransactions, lockPool *pgxpool.Pool, opts ...StaleHostFencerOption) (*StaleHostFencer, error) {
	if transactions == nil {
		return nil, errors.New("stale worker fence transactions are required")
	}
	fencer := &StaleHostFencer{
		transactions: transactions,
		lockPool:     lockPool,
		every:        DefaultStaleHostFenceEvery,
		timeout:      DefaultStaleHostFenceTimeout,
		maxBackoff:   DefaultStaleHostFenceBackoff,
		log:          slog.Default(),
		clock:        systemStaleHostFenceClock{},
	}
	for _, opt := range opts {
		opt(fencer)
	}
	if fencer.every <= 0 {
		return nil, errors.New("stale worker fence interval must be positive")
	}
	if fencer.timeout <= 0 {
		return nil, errors.New("stale worker fence timeout must be positive")
	}
	if fencer.maxBackoff < fencer.every {
		return nil, errors.New("stale worker fence max backoff must be at least the interval")
	}
	if fencer.log == nil {
		fencer.log = slog.Default()
	}
	if fencer.clock == nil {
		return nil, errors.New("stale worker fence clock is required")
	}
	return fencer, nil
}

func (f *StaleHostFencer) Run(ctx context.Context) error {
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		cycleCtx, cancel := context.WithTimeout(ctx, f.timeout)
		cycle, err := f.ReconcileOnce(cycleCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}

		delay := f.every
		if err != nil {
			failures++
			delay = staleHostFenceFailureBackoff(f.every, f.maxBackoff, failures)
			f.log.Error("stale worker fence cycle failed",
				"error", err,
				"consecutive_failures", failures,
				"retry_after", delay,
			)
		} else {
			failures = 0
			f.log.Debug("stale worker fence cycle completed",
				"lock_acquired", cycle.LockAcquired,
				"selected", cycle.Selected,
				"fenced", cycle.Fenced,
				"skipped", cycle.Skipped,
			)
		}
		if err := f.clock.Wait(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			f.log.Warn("stale worker fence retry wait failed", "error", err)
		}
	}
}

func (f *StaleHostFencer) ReconcileOnce(ctx context.Context) (StaleHostFenceCycle, error) {
	cycle := StaleHostFenceCycle{LockAcquired: f.lockPool == nil}
	transactions := f.transactions
	if f.lockPool != nil {
		guard, locked, err := pglock.TryAcquire(ctx, f.lockPool, pglock.Key(staleHostFenceLockName))
		if err != nil {
			return cycle, fmt.Errorf("acquire stale worker fence lock: %w", err)
		}
		if !locked {
			return cycle, nil
		}
		cycle.LockAcquired = true
		transactions = pgxStaleHostFenceTransactions{beginner: guard.Conn()}
		defer func() {
			if err := guard.Unlock(); err != nil {
				f.log.Warn("release stale worker fence lock failed", "error", err)
			}
		}()
	}

	registrationStaleBefore := pgtype.Timestamptz{Time: f.clock.Now().Add(-DefaultWorkerRegistrationReadinessGrace), Valid: true}
	err := transactions.withinStaleHostFenceTransaction(ctx, func(queries staleHostFenceQueries) error {
		candidates, err := queries.ListStaleWorkerFenceCandidates(ctx, db.ListStaleWorkerFenceCandidatesParams{
			RegistrationStaleBefore:     registrationStaleBefore,
			ObservationFreshnessSeconds: ObservationFreshnessSeconds,
			RowLimit:                    DefaultStaleHostFenceBatch,
		})
		if err != nil {
			return fmt.Errorf("select stale worker fence candidates: %w", err)
		}
		cycle.Selected = len(candidates)
		cycle.Results = make([]StaleHostFenceResult, 0, len(candidates))
		for _, candidate := range candidates {
			result := StaleHostFenceResult{
				WorkerHostID:   candidate.ID,
				WorkerGroupID:  candidate.WorkerGroupID,
				WorkerEpoch:    candidate.CurrentEpoch,
				PreviousStatus: candidate.Status,
				FreshnessAt:    candidate.FreshnessAt.Time,
				Reason:         candidate.Reason,
			}
			_, err := queries.RecheckAndFenceStaleWorkerHost(ctx, db.RecheckAndFenceStaleWorkerHostParams{
				ID:                          candidate.ID,
				WorkerGroupID:               candidate.WorkerGroupID,
				ExpectedEpoch:               candidate.CurrentEpoch,
				RegistrationStaleBefore:     registrationStaleBefore,
				ObservationFreshnessSeconds: ObservationFreshnessSeconds,
				ReasonCode:                  pgtype.Text{String: staleHostReasonCode, Valid: true},
			})
			switch {
			case err == nil:
				result.Outcome = StaleHostFenced
				cycle.Fenced++
			case errors.Is(err, pgx.ErrNoRows):
				result.Outcome = StaleHostSkipped
				result.Reason = "fresh_observation_or_worker_changed"
				cycle.Skipped++
			default:
				return fmt.Errorf("fence stale worker %s at epoch %d: %w",
					candidate.ID, candidate.CurrentEpoch.Int64, err)
			}
			cycle.Results = append(cycle.Results, result)
		}
		return nil
	})
	if err != nil {
		return StaleHostFenceCycle{LockAcquired: cycle.LockAcquired}, err
	}
	for _, result := range cycle.Results {
		f.log.Info("stale worker fence result",
			"worker_host_id", result.WorkerHostID,
			"worker_group_id", pgvalue.UUIDString(result.WorkerGroupID),
			"worker_epoch", result.WorkerEpoch.Int64,
			"previous_status", result.PreviousStatus,
			"freshness_at", result.FreshnessAt,
			"outcome", result.Outcome,
			"reason", result.Reason,
		)
	}
	return cycle, nil
}

type pgxStaleHostFenceBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

type pgxStaleHostFenceTransactions struct {
	beginner pgxStaleHostFenceBeginner
}

func (transactions pgxStaleHostFenceTransactions) withinStaleHostFenceTransaction(
	ctx context.Context,
	fn func(staleHostFenceQueries) error,
) error {
	tx, err := transactions.beginner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin stale worker fence transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit stale worker fence transaction: %w", err)
	}
	return nil
}

type systemStaleHostFenceClock struct{}

func (systemStaleHostFenceClock) Now() time.Time { return time.Now() }

func (systemStaleHostFenceClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func staleHostFenceFailureBackoff(every, maximum time.Duration, failures int) time.Duration {
	delay := every
	for step := 1; step < failures && delay < maximum; step++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	return min(delay, maximum)
}
