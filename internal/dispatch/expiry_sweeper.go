package dispatch

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
	DefaultExpirySweepInterval                = 5 * time.Second
	DefaultExpirySweepOrgLimit                = int32(500)
	DefaultExpirySweepConsecutiveFailureLimit = 3
	expirySweepUnlockTimeout                  = 5 * time.Second
)

type ExpirySweepStore interface {
	ExpirySweepOrgStore
	ListOrganizationIDsPage(context.Context, db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error)
}

type ExpirySweepOrgStore interface {
	RequeueExpiredLeasedRunExecutionSessions(ctx context.Context, orgID pgtype.UUID) error
	FailExpiredRunningRunExecutionSessions(ctx context.Context, orgID pgtype.UUID) error
	ExpireQueuedRuns(ctx context.Context, orgID pgtype.UUID) error
	ExpireDuePendingWaitpoints(ctx context.Context, orgID pgtype.UUID) error
}

type ExpirySweepLock interface {
	TryLock(ctx context.Context) (ExpirySweepLockGuard, bool, error)
}

type ExpirySweepLockGuard interface {
	Store(fallback ExpirySweepStore) ExpirySweepStore
	Unlock(ctx context.Context) error
}

type ExpirySweeper struct {
	store        ExpirySweepStore
	lock         ExpirySweepLock
	every        time.Duration
	orgLimit     int32
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

func (s *ExpirySweeper) Run(ctx context.Context) error {
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
			s.log.Warn("sweep expired sessions failed", "error", err, "consecutive_failures", consecutiveFailures)
			if consecutiveFailures >= s.failureLimit {
				return fmt.Errorf("sweep expired sessions failed %d consecutive times: %w", consecutiveFailures, err)
			}
		} else {
			consecutiveFailures = 0
		}
		timer.Reset(s.every)
	}
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

func sweepOnce(ctx context.Context, store ExpirySweepStore, orgLimit int32) error {
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

func SweepExpiredForOrg(ctx context.Context, store ExpirySweepOrgStore, orgID pgtype.UUID) error {
	if err := store.RequeueExpiredLeasedRunExecutionSessions(ctx, orgID); err != nil {
		return err
	}
	if err := store.FailExpiredRunningRunExecutionSessions(ctx, orgID); err != nil {
		return err
	}
	if err := store.ExpireQueuedRuns(ctx, orgID); err != nil {
		return err
	}
	if err := store.ExpireDuePendingWaitpoints(ctx, orgID); err != nil {
		return err
	}
	return nil
}
