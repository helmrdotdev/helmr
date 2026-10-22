package vm

import (
	"context"
	"errors"
)

// StartLimiter permits cover startup only; current-epoch execution slots
// govern steady state.
type StartLimiter struct {
	backend Backend
	permits chan struct{}
}

func NewStartLimiter(backend Backend, maximum int) (*StartLimiter, error) {
	if backend == nil || maximum <= 0 {
		return nil, errors.New("VM start limiter requires a connector and positive maximum")
	}
	return &StartLimiter{backend: backend, permits: make(chan struct{}, maximum)}, nil
}

func (l *StartLimiter) withPermit(ctx context.Context, start func() (CheckpointableMachine, error)) (CheckpointableMachine, error) {
	select {
	case l.permits <- struct{}{}:
		defer func() { <-l.permits }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return start()
}

func (l *StartLimiter) Restore(ctx context.Context, request RestoreRequest) (CheckpointableMachine, error) {
	return l.withPermit(ctx, func() (CheckpointableMachine, error) { return l.backend.Restore(ctx, request) })
}

func (l *StartLimiter) Materialize(ctx context.Context, request MaterializeRequest) (CheckpointableMachine, error) {
	return l.withPermit(ctx, func() (CheckpointableMachine, error) { return l.backend.Materialize(ctx, request) })
}

func (l *StartLimiter) PrepareBootSeed(ctx context.Context, request MaterializeRequest) (BootSeedCapture, error) {
	select {
	case l.permits <- struct{}{}:
		defer func() { <-l.permits }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.backend.PrepareBootSeed(ctx, request)
}

// PrepareComputer shares the startup permit with real allocations and seeds.
func (l *StartLimiter) PrepareComputer(ctx context.Context, request MaterializeRequest) (PreparedComputer, error) {
	backend, ok := l.backend.(PreparedComputerBackend)
	if !ok {
		return nil, errors.New("VM backend does not support prepared Computers")
	}
	select {
	case l.permits <- struct{}{}:
		defer func() { <-l.permits }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return backend.PrepareComputer(ctx, request)
}

func (l *StartLimiter) BootSeedMatches(request MaterializeRequest, manifest []byte) (bool, error) {
	return l.backend.BootSeedMatches(request, manifest)
}

func (l *StartLimiter) Cleanup(ctx context.Context, owner Owner) error {
	return l.backend.Cleanup(ctx, owner)
}
