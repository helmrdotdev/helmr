package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DefaultSweepInterval                = 5 * time.Second
	DefaultSweepOrgLimit                = int32(500)
	DefaultSweepConsecutiveFailureLimit = 3
	sweepUnlockTimeout                  = 5 * time.Second
)

type SweeperStore interface {
	SweeperOrgStore
	ListOrganizationIDsPage(context.Context, db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error)
}

type SweeperOrgStore interface {
	RequeueExpiredLeasedRunExecutions(ctx context.Context, orgID pgtype.UUID) error
	FailExpiredRunningRunExecutions(ctx context.Context, orgID pgtype.UUID) error
	ExpireDuePendingWaitpoints(ctx context.Context, orgID pgtype.UUID) error
}

type SweepLock interface {
	TryLock(ctx context.Context) (SweepLockGuard, bool, error)
}

type SweepLockGuard interface {
	Store(fallback SweeperStore) SweeperStore
	Unlock(ctx context.Context) error
}

type Sweeper struct {
	store        SweeperStore
	lock         SweepLock
	every        time.Duration
	orgLimit     int32
	failureLimit int
	log          *slog.Logger
}

type SweeperOption func(*Sweeper)

func WithSweepInterval(every time.Duration) SweeperOption {
	return func(sweeper *Sweeper) {
		sweeper.every = every
	}
}

func WithSweepOrgLimit(limit int32) SweeperOption {
	return func(sweeper *Sweeper) {
		sweeper.orgLimit = limit
	}
}

func WithSweepConsecutiveFailureLimit(limit int) SweeperOption {
	return func(sweeper *Sweeper) {
		sweeper.failureLimit = limit
	}
}

func WithLogger(log *slog.Logger) SweeperOption {
	return func(sweeper *Sweeper) {
		sweeper.log = log
	}
}

func WithSweepLock(lock SweepLock) SweeperOption {
	return func(sweeper *Sweeper) {
		sweeper.lock = lock
	}
}

func NewSweeper(store SweeperStore, opts ...SweeperOption) (*Sweeper, error) {
	if store == nil {
		return nil, errors.New("sweeper store is required")
	}
	sweeper := &Sweeper{
		store:        store,
		every:        DefaultSweepInterval,
		orgLimit:     DefaultSweepOrgLimit,
		failureLimit: DefaultSweepConsecutiveFailureLimit,
		log:          slog.Default(),
	}
	for _, opt := range opts {
		opt(sweeper)
	}
	if sweeper.every <= 0 {
		return nil, errors.New("sweep interval must be positive")
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

func (s *Sweeper) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := s.sweep(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			consecutiveFailures++
			s.log.Warn("sweep expired executions failed", "error", err, "consecutive_failures", consecutiveFailures)
			if consecutiveFailures >= s.failureLimit {
				return fmt.Errorf("sweep expired executions failed %d consecutive times: %w", consecutiveFailures, err)
			}
		} else {
			consecutiveFailures = 0
		}
		timer.Reset(s.every)
	}
}

func (s *Sweeper) sweep(ctx context.Context) error {
	var guard SweepLockGuard
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
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				s.log.Warn("release sweeper lock failed", "error", err)
			}
		}()
	}
	return sweepOnce(ctx, store, s.orgLimit)
}

func sweepOnce(ctx context.Context, store SweeperStore, orgLimit int32) error {
	var problems []error
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
			if err := SweepOnceForOrg(ctx, store, orgID); err != nil {
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

func SweepOnceForOrg(ctx context.Context, store SweeperOrgStore, orgID pgtype.UUID) error {
	if err := store.RequeueExpiredLeasedRunExecutions(ctx, orgID); err != nil {
		return err
	}
	if err := store.FailExpiredRunningRunExecutions(ctx, orgID); err != nil {
		return err
	}
	if err := store.ExpireDuePendingWaitpoints(ctx, orgID); err != nil {
		return err
	}
	return nil
}
