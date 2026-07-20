package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultExpirySweepInterval                = 5 * time.Second
	DefaultExpirySweepTimeout                 = 30 * time.Second
	DefaultExpirySweepConsecutiveFailureLimit = 3
	expirySweepUnlockTimeout                  = 5 * time.Second
	buildExpirySweepLockName                  = "helmr.dispatcher.build_expiry_sweeper"
)

type BuildExpirySweepStore interface {
	RequeueExpiredDeploymentBuildLeases(context.Context) error
}

type BuildExpirySweepLock interface {
	TryLock(context.Context) (BuildExpirySweepLockGuard, bool, error)
}

type BuildExpirySweepLockGuard interface {
	Store(BuildExpirySweepStore) BuildExpirySweepStore
	Unlock(context.Context) error
}

type BuildExpirySweeper struct {
	store        BuildExpirySweepStore
	lock         BuildExpirySweepLock
	every        time.Duration
	timeout      time.Duration
	failureLimit int
	log          *slog.Logger
}

type BuildExpirySweeperOption func(*BuildExpirySweeper)

func WithBuildExpirySweepInterval(every time.Duration) BuildExpirySweeperOption {
	return func(sweeper *BuildExpirySweeper) {
		sweeper.every = every
	}
}

func WithBuildExpirySweepTimeout(timeout time.Duration) BuildExpirySweeperOption {
	return func(sweeper *BuildExpirySweeper) {
		sweeper.timeout = timeout
	}
}

func WithBuildExpirySweepConsecutiveFailureLimit(limit int) BuildExpirySweeperOption {
	return func(sweeper *BuildExpirySweeper) {
		sweeper.failureLimit = limit
	}
}

func WithBuildExpirySweepLogger(log *slog.Logger) BuildExpirySweeperOption {
	return func(sweeper *BuildExpirySweeper) {
		sweeper.log = log
	}
}

func WithBuildExpirySweepLock(lock BuildExpirySweepLock) BuildExpirySweeperOption {
	return func(sweeper *BuildExpirySweeper) {
		sweeper.lock = lock
	}
}

func NewBuildExpirySweeper(store BuildExpirySweepStore, opts ...BuildExpirySweeperOption) (*BuildExpirySweeper, error) {
	if store == nil {
		return nil, errors.New("build sweeper store is required")
	}
	sweeper := &BuildExpirySweeper{
		store:        store,
		every:        DefaultExpirySweepInterval,
		timeout:      DefaultExpirySweepTimeout,
		failureLimit: DefaultExpirySweepConsecutiveFailureLimit,
		log:          slog.Default(),
	}
	for _, opt := range opts {
		opt(sweeper)
	}
	if sweeper.every <= 0 {
		return nil, errors.New("build sweep interval must be positive")
	}
	if sweeper.timeout <= 0 {
		return nil, errors.New("build sweep timeout must be positive")
	}
	if sweeper.failureLimit <= 0 {
		return nil, errors.New("build sweep consecutive failure limit must be positive")
	}
	if sweeper.log == nil {
		sweeper.log = slog.Default()
	}
	return sweeper, nil
}

func (s *BuildExpirySweeper) Run(ctx context.Context) error {
	return runExpiryLoop(ctx, "build", s.every, s.timeout, s.failureLimit, s.log, s.sweep)
}

func runExpiryLoop(
	ctx context.Context,
	domain string,
	every time.Duration,
	timeout time.Duration,
	failureLimit int,
	log *slog.Logger,
	sweep func(context.Context) error,
) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}

		sweepCtx, cancel := context.WithTimeout(ctx, timeout)
		err := sweep(sweepCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}

		delay := every
		if err != nil {
			consecutiveFailures++
			delay = expiryFailureBackoff(every, consecutiveFailures, failureLimit)
			logFn := log.Warn
			if consecutiveFailures >= failureLimit {
				logFn = log.Error
			}
			logFn("expiry sweep failed",
				"domain", domain,
				"error", err,
				"consecutive_failures", consecutiveFailures,
				"retry_after", delay,
			)
		} else {
			consecutiveFailures = 0
		}
		timer.Reset(delay)
	}
}

func expiryFailureBackoff(every time.Duration, consecutiveFailures, failureLimit int) time.Duration {
	steps := min(consecutiveFailures, failureLimit) - 1
	delay := every
	for range steps {
		if delay > time.Duration(1<<62)/2 {
			return time.Duration(1 << 62)
		}
		delay *= 2
	}
	return delay
}

func (s *BuildExpirySweeper) sweep(ctx context.Context) error {
	var guard BuildExpirySweepLockGuard
	store := s.store
	if s.lock != nil {
		var locked bool
		var err error
		guard, locked, err = s.lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !locked {
			s.log.Debug("build expiry sweeper lock is held by another instance")
			return nil
		}
		store = guard.Store(s.store)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), expirySweepUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				s.log.Warn("release build expiry sweeper lock failed", "error", err)
			}
		}()
	}
	return store.RequeueExpiredDeploymentBuildLeases(ctx)
}

type BuildExpirySweepAdvisoryLock struct {
	lock *advisoryLock
}

func NewBuildExpirySweepAdvisoryLock(pool *pgxpool.Pool) (*BuildExpirySweepAdvisoryLock, error) {
	lock, err := newAdvisoryLock(pool, buildExpirySweepLockName)
	if err != nil {
		return nil, err
	}
	return &BuildExpirySweepAdvisoryLock{lock: lock}, nil
}

func (l *BuildExpirySweepAdvisoryLock) TryLock(ctx context.Context) (BuildExpirySweepLockGuard, bool, error) {
	guard, locked, err := l.lock.tryLock(ctx)
	if err != nil || !locked {
		return nil, locked, err
	}
	return buildExpirySweepAdvisoryLockGuard{guard: guard}, true, nil
}

type buildExpirySweepAdvisoryLockGuard struct {
	guard advisoryLockGuard
}

func (g buildExpirySweepAdvisoryLockGuard) Store(BuildExpirySweepStore) BuildExpirySweepStore {
	return db.New(g.guard.conn)
}

func (g buildExpirySweepAdvisoryLockGuard) Unlock(ctx context.Context) error {
	return g.guard.Unlock(ctx)
}
