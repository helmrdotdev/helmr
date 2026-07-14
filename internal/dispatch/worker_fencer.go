package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultStaleWorkerGrace                 = 2 * time.Minute
	DefaultWorkerRegistrationReadinessGrace = 15 * time.Minute
	DefaultStaleWorkerFenceBatch            = int32(100)
	DefaultStaleWorkerFenceEvery            = 5 * time.Second
	DefaultStaleWorkerFenceTimeout          = 30 * time.Second
	DefaultStaleWorkerFenceBackoff          = 30 * time.Second

	staleWorkerFenceLockName = "helmr.dispatcher.stale_worker_fencer"
	staleWorkerReasonCode    = "worker_observation_stale"
)

// StaleWorkerFenceQueries is the query surface used while candidate worker
// rows remain locked. The second query deliberately executes as a separate
// statement so READ COMMITTED sees an observation that won the worker-row
// lock immediately before this transaction.
type StaleWorkerFenceQueries interface {
	ListStaleWorkerFenceCandidates(context.Context, db.ListStaleWorkerFenceCandidatesParams) ([]db.ListStaleWorkerFenceCandidatesRow, error)
	RecheckAndFenceStaleWorkerInstance(context.Context, db.RecheckAndFenceStaleWorkerInstanceParams) (db.RecheckAndFenceStaleWorkerInstanceRow, error)
}

type StaleWorkerFenceTransactions interface {
	WithinStaleWorkerFenceTransaction(context.Context, func(StaleWorkerFenceQueries) error) error
}

type StaleWorkerFenceLock interface {
	TryLock(context.Context) (StaleWorkerFenceLockGuard, bool, error)
}

type StaleWorkerFenceLockGuard interface {
	Transactions(StaleWorkerFenceTransactions) StaleWorkerFenceTransactions
	Unlock(context.Context) error
}

type StaleWorkerFenceClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type StaleWorkerFenceOutcome string

const (
	StaleWorkerFenced  StaleWorkerFenceOutcome = "fenced"
	StaleWorkerSkipped StaleWorkerFenceOutcome = "skipped"
)

type StaleWorkerFenceResult struct {
	WorkerInstanceID pgtype.UUID
	WorkerGroupID    string
	WorkerEpoch      pgtype.Int8
	PreviousState    db.WorkerInstanceState
	FreshnessAt      time.Time
	Outcome          StaleWorkerFenceOutcome
	Reason           string
}

type StaleWorkerFenceCycle struct {
	LockAcquired bool
	Selected     int
	Fenced       int
	Skipped      int
	Results      []StaleWorkerFenceResult
}

type StaleWorkerFencer struct {
	transactions      StaleWorkerFenceTransactions
	lock              StaleWorkerFenceLock
	grace             time.Duration
	registrationGrace time.Duration
	groupGrace        map[string]WorkerGroupFenceGrace
	batch             int32
	every             time.Duration
	timeout           time.Duration
	maxBackoff        time.Duration
	log               *slog.Logger
	clock             StaleWorkerFenceClock
}

type StaleWorkerFencerOption func(*StaleWorkerFencer)

type WorkerGroupFenceGrace struct {
	Observation  time.Duration
	Registration time.Duration
}

func WithStaleWorkerFenceLock(lock StaleWorkerFenceLock) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.lock = lock }
}

func WithStaleWorkerGrace(grace time.Duration) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.grace = grace }
}

func WithWorkerRegistrationReadinessGrace(grace time.Duration) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.registrationGrace = grace }
}

func WithWorkerGroupFenceGrace(grace map[string]WorkerGroupFenceGrace) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) {
		fencer.groupGrace = make(map[string]WorkerGroupFenceGrace, len(grace))
		maps.Copy(fencer.groupGrace, grace)
	}
}

func WithStaleWorkerFenceBatch(batch int32) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.batch = batch }
}

func WithStaleWorkerFenceInterval(every time.Duration) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.every = every }
}

func WithStaleWorkerFenceTimeout(timeout time.Duration) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.timeout = timeout }
}

func WithStaleWorkerFenceMaxBackoff(maxBackoff time.Duration) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.maxBackoff = maxBackoff }
}

func WithStaleWorkerFenceLogger(log *slog.Logger) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.log = log }
}

func WithStaleWorkerFenceClock(clock StaleWorkerFenceClock) StaleWorkerFencerOption {
	return func(fencer *StaleWorkerFencer) { fencer.clock = clock }
}

func NewStaleWorkerFencer(transactions StaleWorkerFenceTransactions, opts ...StaleWorkerFencerOption) (*StaleWorkerFencer, error) {
	if transactions == nil {
		return nil, errors.New("stale worker fence transactions are required")
	}
	fencer := &StaleWorkerFencer{
		transactions:      transactions,
		grace:             DefaultStaleWorkerGrace,
		registrationGrace: DefaultWorkerRegistrationReadinessGrace,
		batch:             DefaultStaleWorkerFenceBatch,
		every:             DefaultStaleWorkerFenceEvery,
		timeout:           DefaultStaleWorkerFenceTimeout,
		maxBackoff:        DefaultStaleWorkerFenceBackoff,
		log:               slog.Default(),
		clock:             systemStaleWorkerFenceClock{},
	}
	for _, opt := range opts {
		opt(fencer)
	}
	if fencer.grace <= 0 {
		return nil, errors.New("stale worker grace must be positive")
	}
	if fencer.registrationGrace <= 0 {
		return nil, errors.New("worker registration grace must be positive")
	}
	for groupID, grace := range fencer.groupGrace {
		if groupID == "" || grace.Observation <= 0 || grace.Registration <= 0 {
			return nil, errors.New("worker group fence grace requires a group ID and positive durations")
		}
	}
	if fencer.batch <= 0 {
		return nil, errors.New("stale worker fence batch must be positive")
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

func (f *StaleWorkerFencer) Run(ctx context.Context) error {
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
			delay = staleWorkerFenceFailureBackoff(f.every, f.maxBackoff, failures)
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

func (f *StaleWorkerFencer) ReconcileOnce(ctx context.Context) (StaleWorkerFenceCycle, error) {
	cycle := StaleWorkerFenceCycle{LockAcquired: f.lock == nil}
	transactions := f.transactions
	if f.lock != nil {
		guard, locked, err := f.lock.TryLock(ctx)
		if err != nil {
			return cycle, fmt.Errorf("acquire stale worker fence lock: %w", err)
		}
		if !locked {
			return cycle, nil
		}
		cycle.LockAcquired = true
		transactions = guard.Transactions(f.transactions)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), expirySweepUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				f.log.Warn("release stale worker fence lock failed", "error", err)
			}
		}()
	}

	now := f.clock.Now()
	type fenceScope struct {
		groupID      string
		observation  time.Duration
		registration time.Duration
	}
	scopes := make([]fenceScope, 0, max(1, len(f.groupGrace)))
	if len(f.groupGrace) == 0 {
		scopes = append(scopes, fenceScope{observation: f.grace, registration: f.registrationGrace})
	} else {
		groupIDs := make([]string, 0, len(f.groupGrace))
		for groupID := range f.groupGrace {
			groupIDs = append(groupIDs, groupID)
		}
		sort.Strings(groupIDs)
		for _, groupID := range groupIDs {
			grace := f.groupGrace[groupID]
			scopes = append(scopes, fenceScope{groupID: groupID, observation: grace.Observation, registration: grace.Registration})
		}
	}
	err := transactions.WithinStaleWorkerFenceTransaction(ctx, func(queries StaleWorkerFenceQueries) error {
		cycle.Results = make([]StaleWorkerFenceResult, 0, int(f.batch)*len(scopes))
		for _, scope := range scopes {
			observationStaleBefore := now.Add(-scope.observation)
			registrationStaleBefore := now.Add(-scope.registration)
			candidates, err := queries.ListStaleWorkerFenceCandidates(ctx, db.ListStaleWorkerFenceCandidatesParams{
				WorkerGroupID:           scope.groupID,
				ObservationStaleBefore:  pgtype.Timestamptz{Time: observationStaleBefore, Valid: true},
				RegistrationStaleBefore: pgtype.Timestamptz{Time: registrationStaleBefore, Valid: true},
				RowLimit:                f.batch,
			})
			if err != nil {
				return fmt.Errorf("select stale worker fence candidates for group %q: %w", scope.groupID, err)
			}
			cycle.Selected += len(candidates)
			for _, candidate := range candidates {
				result := StaleWorkerFenceResult{
					WorkerInstanceID: candidate.ID,
					WorkerGroupID:    candidate.WorkerGroupID,
					WorkerEpoch:      candidate.CurrentEpoch,
					PreviousState:    candidate.State,
					FreshnessAt:      candidate.FreshnessAt.Time,
					Reason:           candidate.Reason,
				}
				_, err := queries.RecheckAndFenceStaleWorkerInstance(ctx, db.RecheckAndFenceStaleWorkerInstanceParams{
					ID:                      candidate.ID,
					WorkerGroupID:           candidate.WorkerGroupID,
					ExpectedEpoch:           candidate.CurrentEpoch,
					ObservationStaleBefore:  pgtype.Timestamptz{Time: observationStaleBefore, Valid: true},
					RegistrationStaleBefore: pgtype.Timestamptz{Time: registrationStaleBefore, Valid: true},
					ReasonCode:              pgtype.Text{String: staleWorkerReasonCode, Valid: true},
				})
				switch {
				case err == nil:
					result.Outcome = StaleWorkerFenced
					cycle.Fenced++
				case errors.Is(err, pgx.ErrNoRows):
					result.Outcome = StaleWorkerSkipped
					result.Reason = "fresh_observation_or_worker_changed"
					cycle.Skipped++
				default:
					return fmt.Errorf("fence stale worker %s at epoch %d: %w",
						candidate.ID, candidate.CurrentEpoch.Int64, err)
				}
				cycle.Results = append(cycle.Results, result)
			}
		}
		return nil
	})
	if err != nil {
		return StaleWorkerFenceCycle{LockAcquired: cycle.LockAcquired}, err
	}
	for _, result := range cycle.Results {
		f.log.Info("stale worker fence result",
			"worker_instance_id", result.WorkerInstanceID,
			"worker_group_id", result.WorkerGroupID,
			"worker_epoch", result.WorkerEpoch.Int64,
			"previous_state", result.PreviousState,
			"freshness_at", result.FreshnessAt,
			"outcome", result.Outcome,
			"reason", result.Reason,
		)
	}
	return cycle, nil
}

type pgxStaleWorkerFenceBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

type pgxStaleWorkerFenceTransactions struct {
	beginner pgxStaleWorkerFenceBeginner
}

func NewPGXStaleWorkerFenceTransactions(pool *pgxpool.Pool) (StaleWorkerFenceTransactions, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	return pgxStaleWorkerFenceTransactions{beginner: pool}, nil
}

func (transactions pgxStaleWorkerFenceTransactions) WithinStaleWorkerFenceTransaction(
	ctx context.Context,
	fn func(StaleWorkerFenceQueries) error,
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

type StaleWorkerFenceAdvisoryLock struct {
	lock *ExpirySweepAdvisoryLock
}

func NewStaleWorkerFenceAdvisoryLock(pool *pgxpool.Pool) (*StaleWorkerFenceAdvisoryLock, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	return &StaleWorkerFenceAdvisoryLock{lock: &ExpirySweepAdvisoryLock{
		pool: pool,
		key:  advisoryLockKey(staleWorkerFenceLockName),
	}}, nil
}

func (lock *StaleWorkerFenceAdvisoryLock) TryLock(ctx context.Context) (StaleWorkerFenceLockGuard, bool, error) {
	guard, locked, err := lock.lock.tryLock(ctx)
	if err != nil || !locked {
		return nil, locked, err
	}
	return staleWorkerFenceAdvisoryLockGuard{guard: guard}, true, nil
}

type staleWorkerFenceAdvisoryLockGuard struct {
	guard advisoryLockGuard
}

func (guard staleWorkerFenceAdvisoryLockGuard) Transactions(StaleWorkerFenceTransactions) StaleWorkerFenceTransactions {
	return pgxStaleWorkerFenceTransactions{beginner: guard.guard.conn}
}

func (guard staleWorkerFenceAdvisoryLockGuard) Unlock(ctx context.Context) error {
	return guard.guard.Unlock(ctx)
}

type systemStaleWorkerFenceClock struct{}

func (systemStaleWorkerFenceClock) Now() time.Time { return time.Now() }

func (systemStaleWorkerFenceClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func staleWorkerFenceFailureBackoff(every, maximum time.Duration, failures int) time.Duration {
	delay := every
	for step := 1; step < failures && delay < maximum; step++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	return min(delay, maximum)
}
