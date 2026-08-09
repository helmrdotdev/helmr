package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RunPlacementLaneGuard interface {
	Discovery() RunPlacementDiscovery
	Unlock() error
}

type RunPlacementLaneLocker interface {
	TryLock(context.Context, int16) (RunPlacementLaneGuard, bool, error)
}

type RunPlacementLaneLock struct {
	pool *pgxpool.Pool
}

func NewRunPlacementLaneLock(pool *pgxpool.Pool) (*RunPlacementLaneLock, error) {
	if pool == nil {
		return nil, errors.New("run placement lane lock pool is required")
	}
	return &RunPlacementLaneLock{pool: pool}, nil
}

func (l *RunPlacementLaneLock) TryLock(
	ctx context.Context,
	lane int16,
) (RunPlacementLaneGuard, bool, error) {
	if lane < 0 || lane >= runPlacementLaneCount {
		return nil, false, errors.New("run placement lane is out of range")
	}
	guard, locked, err := pglock.TryAcquire(ctx, l.pool, runPlacementLaneLockKey(lane))
	if err != nil || !locked {
		return nil, locked, err
	}
	store, err := NewRunPlacementStore(guard.Conn())
	if err != nil {
		return nil, false, errors.Join(err, guard.Unlock())
	}
	return &runPlacementLaneGuard{guard: guard, discovery: store}, true, nil
}

func runPlacementLaneLockKey(lane int16) int64 {
	return pglock.Key(fmt.Sprintf("helmr.dispatcher.run_placement_lane.%d", lane))
}

type runPlacementLaneGuard struct {
	guard     *pglock.Guard
	discovery RunPlacementDiscovery
}

func (g *runPlacementLaneGuard) Discovery() RunPlacementDiscovery {
	return g.discovery
}

func (g *runPlacementLaneGuard) Unlock() error {
	if g == nil || g.guard == nil {
		return errors.New("run placement lane guard is already released")
	}
	guard := g.guard
	g.guard = nil
	g.discovery = nil
	return guard.Unlock()
}
