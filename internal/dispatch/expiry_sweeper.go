package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultExpirySweepInterval                = 5 * time.Second
	DefaultExpirySweepTimeout                 = 30 * time.Second
	DefaultExpirySweepOrgLimit                = int32(500)
	DefaultExpirySweepConsecutiveFailureLimit = 3
	expirySweepUnlockTimeout                  = 5 * time.Second
	buildExpirySweepLockName                  = "helmr.dispatcher.build_expiry_sweeper"
)

type ExpirySweepStore interface {
	ExpirySweepOrgStore
	ListOrganizationIDsPage(context.Context, db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error)
	MarkExpiredRuntimeInstancesLost(ctx context.Context, limitCount int32) ([]db.RuntimeInstance, error)
	MarkStaleWorkspaceMountsLost(ctx context.Context, limitCount int32) ([]db.WorkspaceMount, error)
	ExpireWorkspaceLeases(ctx context.Context, limitCount int32) ([]db.ExpireWorkspaceLeasesRow, error)
}

type BuildExpirySweepStore interface {
	RequeueExpiredDeploymentBuildLeases(ctx context.Context) error
}

type ExpirySweepOrgStore interface {
	RequeueExpiredLeasedRunLeases(ctx context.Context) error
	RequeueExpiredRunningRunLeases(ctx context.Context) error
	ExpireQueuedRuns(ctx context.Context, orgID pgtype.UUID) error
	ExpireDueSessions(ctx context.Context, orgID pgtype.UUID) ([]db.Session, error)
	ExpireDueTokens(ctx context.Context, orgID pgtype.UUID) ([]db.ExpireDueTokensRow, error)
	ResolveDueTimerWaits(ctx context.Context, arg db.ResolveDueTimerWaitsParams) ([]db.ResolveDueTimerWaitsRow, error)
	ExpireDueRunWaits(ctx context.Context, limitCount int32) ([]db.RunWait, error)
	RequeueStaleResumingRunWaits(ctx context.Context, arg db.RequeueStaleResumingRunWaitsParams) ([]db.RequeueStaleResumingRunWaitsRow, error)
	RequeueResolvedRunWaits(ctx context.Context, arg db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error)
}

type ExpirySweepLock interface {
	TryLock(ctx context.Context) (ExpirySweepLockGuard, bool, error)
}

type ExpirySweepLockGuard interface {
	Store(fallback ExpirySweepStore) ExpirySweepStore
	Unlock(ctx context.Context) error
}

type BuildExpirySweepLock interface {
	TryLock(ctx context.Context) (BuildExpirySweepLockGuard, bool, error)
}

type BuildExpirySweepLockGuard interface {
	Store(fallback BuildExpirySweepStore) BuildExpirySweepStore
	Unlock(ctx context.Context) error
}

type ExpirySweeper struct {
	store        ExpirySweepStore
	lock         ExpirySweepLock
	every        time.Duration
	timeout      time.Duration
	orgLimit     int32
	failureLimit int
	log          *slog.Logger
}

type BuildExpirySweeper struct {
	store        BuildExpirySweepStore
	lock         BuildExpirySweepLock
	every        time.Duration
	timeout      time.Duration
	failureLimit int
	log          *slog.Logger
}

type ExpirySweeperOption func(*ExpirySweeper)

func WithExpirySweepInterval(every time.Duration) ExpirySweeperOption {
	return func(sweeper *ExpirySweeper) {
		sweeper.every = every
	}
}

func WithExpirySweepOrgLimit(limit int32) ExpirySweeperOption {
	return func(sweeper *ExpirySweeper) {
		sweeper.orgLimit = limit
	}
}

func WithExpirySweepTimeout(timeout time.Duration) ExpirySweeperOption {
	return func(sweeper *ExpirySweeper) {
		sweeper.timeout = timeout
	}
}

func WithExpirySweepConsecutiveFailureLimit(limit int) ExpirySweeperOption {
	return func(sweeper *ExpirySweeper) {
		sweeper.failureLimit = limit
	}
}

func WithExpirySweepLogger(log *slog.Logger) ExpirySweeperOption {
	return func(sweeper *ExpirySweeper) {
		sweeper.log = log
	}
}

func WithExpirySweepLock(lock ExpirySweepLock) ExpirySweeperOption {
	return func(sweeper *ExpirySweeper) {
		sweeper.lock = lock
	}
}

func NewExpirySweeper(store ExpirySweepStore, opts ...ExpirySweeperOption) (*ExpirySweeper, error) {
	if store == nil {
		return nil, errors.New("sweeper store is required")
	}
	sweeper := &ExpirySweeper{
		store:        store,
		every:        DefaultExpirySweepInterval,
		timeout:      DefaultExpirySweepTimeout,
		orgLimit:     DefaultExpirySweepOrgLimit,
		failureLimit: DefaultExpirySweepConsecutiveFailureLimit,
		log:          slog.Default(),
	}
	for _, opt := range opts {
		opt(sweeper)
	}
	if sweeper.every <= 0 {
		return nil, errors.New("sweep interval must be positive")
	}
	if sweeper.timeout <= 0 {
		return nil, errors.New("sweep timeout must be positive")
	}
	if sweeper.orgLimit <= 0 {
		return nil, errors.New("sweep org limit must be positive")
	}
	if sweeper.failureLimit <= 0 {
		return nil, errors.New("sweep consecutive failure limit must be positive")
	}
	if sweeper.log == nil {
		sweeper.log = slog.Default()
	}
	return sweeper, nil
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

func (s *ExpirySweeper) Run(ctx context.Context) error {
	return runExpiryLoop(ctx, "run/runtime/workspace", s.every, s.timeout, s.failureLimit, s.log, s.sweep)
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

func (s *ExpirySweeper) sweep(ctx context.Context) error {
	var guard ExpirySweepLockGuard
	store := s.store
	if s.lock != nil {
		var locked bool
		var err error
		guard, locked, err = s.lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !locked {
			s.log.Debug("sweeper lock is held by another instance")
			return nil
		}
		store = guard.Store(s.store)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), expirySweepUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				s.log.Warn("release sweeper lock failed", "error", err)
			}
		}()
	}
	return sweepOnce(ctx, store, s.orgLimit)
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

func sweepOnce(ctx context.Context, store ExpirySweepStore, orgLimit int32) error {
	var problems []error
	if _, err := store.MarkExpiredRuntimeInstancesLost(ctx, 1000); err != nil {
		problems = append(problems, err)
	}
	if _, err := store.MarkStaleWorkspaceMountsLost(ctx, 1000); err != nil {
		problems = append(problems, err)
	}
	if _, err := store.ExpireWorkspaceLeases(ctx, 1000); err != nil {
		problems = append(problems, err)
	}
	if err := store.RequeueExpiredLeasedRunLeases(ctx); err != nil {
		problems = append(problems, err)
	}
	if err := store.RequeueExpiredRunningRunLeases(ctx); err != nil {
		problems = append(problems, err)
	}
	var afterID pgtype.UUID
	for {
		orgIDs, err := store.ListOrganizationIDsPage(ctx, db.ListOrganizationIDsPageParams{
			AfterID:  afterID,
			RowLimit: orgLimit,
		})
		if err != nil {
			return err
		}
		for _, orgID := range orgIDs {
			if err := SweepExpiredForOrg(ctx, store, orgID); err != nil {
				problems = append(problems, err)
			}
		}
		if len(orgIDs) < int(orgLimit) {
			break
		}
		afterID = orgIDs[len(orgIDs)-1]
	}
	return errors.Join(problems...)
}

type BuildExpirySweepAdvisoryLock struct {
	lock *ExpirySweepAdvisoryLock
}

func NewBuildExpirySweepAdvisoryLock(pool *pgxpool.Pool) (*BuildExpirySweepAdvisoryLock, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	return &BuildExpirySweepAdvisoryLock{
		lock: &ExpirySweepAdvisoryLock{
			pool: pool,
			key:  advisoryLockKey(buildExpirySweepLockName),
		},
	}, nil
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

func SweepExpiredForOrg(ctx context.Context, store ExpirySweepOrgStore, orgID pgtype.UUID) error {
	if err := store.ExpireQueuedRuns(ctx, orgID); err != nil {
		return err
	}
	if _, err := store.ExpireDueSessions(ctx, orgID); err != nil {
		return err
	}
	if _, err := store.ExpireDueTokens(ctx, orgID); err != nil {
		return err
	}
	if _, err := store.ResolveDueTimerWaits(ctx, db.ResolveDueTimerWaitsParams{
		OrgID:      orgID,
		LimitCount: 1000,
	}); err != nil {
		return err
	}
	if _, err := store.ExpireDueRunWaits(ctx, 1000); err != nil {
		return err
	}
	if _, err := store.RequeueStaleResumingRunWaits(ctx, db.RequeueStaleResumingRunWaitsParams{
		OrgID:      orgID,
		StaleAfter: pgtype.Interval{Microseconds: (5 * time.Minute).Microseconds(), Valid: true},
		LimitCount: 1000,
	}); err != nil {
		return err
	}
	if _, err := store.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      orgID,
		LimitCount: 1000,
	}); err != nil {
		return err
	}
	return nil
}
