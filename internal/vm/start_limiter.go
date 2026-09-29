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

func (l *StartLimiter) withPermit(ctx context.Context, start func() (Machine, error)) (Machine, error) {
	select {
	case l.permits <- struct{}{}:
		defer func() { <-l.permits }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return start()
}

func (l *StartLimiter) Restore(ctx context.Context, request RestoreRequest) (Machine, error) {
	return l.withPermit(ctx, func() (Machine, error) { return l.backend.Restore(ctx, request) })
}

func (l *StartLimiter) Materialize(ctx context.Context, request MaterializeRequest) (Machine, error) {
	return l.withPermit(ctx, func() (Machine, error) { return l.backend.Materialize(ctx, request) })
}

func (l *StartLimiter) Cleanup(ctx context.Context, owner Owner) error {
	return l.backend.Cleanup(ctx, owner)
}
