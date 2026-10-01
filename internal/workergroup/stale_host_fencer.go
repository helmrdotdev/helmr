package workergroup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
	Suspended        bool
	SuspensionReason string
	LockAcquired     bool
	Selected         int
	Fenced           int
	Skipped          int
	Results          []StaleHostFenceResult
}

// StaleHostFencer fences worker hosts whose observations went stale. One
// dispatcher replica at a time runs a cycle, serialized by a session-level
// advisory lock; the cycle's READ COMMITTED transaction runs on the connection
// that holds the lock.
type StaleHostFencer struct {
	mu                sync.Mutex
	probeServing      func(context.Context) error
	servingSince      time.Time
	lastServingAt     time.Time
	suspensionReason  string
	lastSuspensionLog time.Time
	transactions      staleHostFenceTransactions
	lockPool          *pgxpool.Pool
	every             time.Duration
	timeout           time.Duration
	maxBackoff        time.Duration
	log               *slog.Logger
	clock             StaleHostFenceClock
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

func NewStaleHostFencer(pool *pgxpool.Pool, controlPlaneURL string, opts ...StaleHostFencerOption) (*StaleHostFencer, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	probe, err := controlPlaneServingProbe(controlPlaneURL)
	if err != nil {
		return nil, err
	}
	return newStaleHostFencer(pgxStaleHostFenceTransactions{beginner: pool}, pool, probe, opts...)
}

// newStaleHostFencer runs cycles without the advisory lock when lockPool is nil.
func newStaleHostFencer(transactions staleHostFenceTransactions, lockPool *pgxpool.Pool, probe func(context.Context) error, opts ...StaleHostFencerOption) (*StaleHostFencer, error) {
	if transactions == nil {
		return nil, errors.New("stale worker fence transactions are required")
	}
	if probe == nil {
		return nil, errors.New("control plane serving probe is required")
	}
	fencer := &StaleHostFencer{
		probeServing: probe,
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
	f.mu.Lock()
	defer f.mu.Unlock()
	cycle := StaleHostFenceCycle{}
	// Every replica collects its own evidence before competing for the cycle lock.
	// A lock transfer never transfers a healthy window.
	if err := f.probeServing(ctx); err != nil {
		f.servingSince = time.Time{}
		f.lastServingAt = time.Time{}
		return f.suspendCycle(cycle, "control plane unavailable", "error", err), nil
	}
	now := f.clock.Now()
	if f.lastServingAt.IsZero() || !servingSampleFresh(now.Sub(f.lastServingAt), now.Round(0).Sub(f.lastServingAt.Round(0)), f.servingEvidenceMaxAge()) {
		f.servingSince = now
	}
	f.lastServingAt = now
	if !f.servingWindowReady(now) {
		return f.suspendCycle(cycle, "waiting for control plane recovery window"), nil
	}
	if f.suspensionReason != "" {
		f.log.Info("stale worker fencing resumed", "healthy_since", f.servingSince)
		f.suspensionReason = ""
	}
	cycle.LockAcquired = f.lockPool == nil
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

	// Do not let a slow transaction outlive the evidence that admitted it.
	transactionCtx, cancel := context.WithTimeout(ctx, f.servingEvidenceMaxAge()-f.clock.Now().Sub(f.lastServingAt))
	defer cancel()
	ctx = transactionCtx
	registrationStaleBefore := pgtype.Timestamptz{Time: f.clock.Now().Add(-DefaultWorkerRegistrationReadinessGrace), Valid: true}
	err := transactions.withinStaleHostFenceTransaction(ctx, func(queries staleHostFenceQueries) error {
		if !f.servingWindowReady(f.clock.Now()) {
			return errServingEvidenceExpired
		}
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
		if !f.servingWindowReady(f.clock.Now()) {
			return errServingEvidenceExpired
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errServingEvidenceExpired) || (!f.servingWindowReady(f.clock.Now()) && ctx.Err() != nil) {
			f.servingSince = time.Time{}
			f.lastServingAt = time.Time{}
			cycle = f.suspendCycle(StaleHostFenceCycle{LockAcquired: cycle.LockAcquired}, "control plane serving evidence expired during fence transaction")
			if errors.Is(err, errServingEvidenceExpired) {
				return cycle, nil
			}
			// A failed COMMIT response may follow durable changes. Preserve the
			// error instead of presenting an uncertain outcome as a safe pause.
			return cycle, err
		}
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

type pgxStaleHostFenceTransactions struct {
	beginner db.TxBeginner
}

func (transactions pgxStaleHostFenceTransactions) withinStaleHostFenceTransaction(
	ctx context.Context,
	fn func(staleHostFenceQueries) error,
) error {
	tx, err := transactions.beginner.Begin(ctx)
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
