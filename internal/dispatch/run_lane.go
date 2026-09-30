package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RunLaneGuard interface {
	Discovery() RunDiscovery
	Unlock() error
}

type RunLaneLocker interface {
	TryLock(context.Context, int16) (RunLaneGuard, bool, error)
}

type RunLaneLock struct {
	pool *pgxpool.Pool
}

func NewRunLaneLock(pool *pgxpool.Pool) (*RunLaneLock, error) {
	if pool == nil {
		return nil, errors.New("run dispatch lane lock pool is required")
	}
	return &RunLaneLock{pool: pool}, nil
}

func (l *RunLaneLock) TryLock(
	ctx context.Context,
	lane int16,
) (RunLaneGuard, bool, error) {
	if lane < 0 || lane >= runLaneCount {
		return nil, false, errors.New("run dispatch lane is out of range")
	}
	guard, locked, err := pglock.TryAcquire(ctx, l.pool, runLaneLockKey(lane))
	if err != nil || !locked {
		return nil, locked, err
	}
	store, err := NewRunStore(guard.Conn())
	if err != nil {
		return nil, false, errors.Join(err, guard.Unlock())
	}
	return &runLaneGuard{guard: guard, discovery: store}, true, nil
}

func runLaneLockKey(lane int16) int64 {
	return pglock.Key(fmt.Sprintf("helmr.dispatcher.run_lane.%d", lane))
}

type runLaneGuard struct {
	guard     *pglock.Guard
	discovery RunDiscovery
}

func (g *runLaneGuard) Discovery() RunDiscovery {
	return g.discovery
}

func (g *runLaneGuard) Unlock() error {
	if g == nil || g.guard == nil {
		return errors.New("run dispatch lane guard is already released")
	}
	guard := g.guard
	g.guard = nil
	g.discovery = nil
	return guard.Unlock()
}
