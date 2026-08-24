package vm

import (
	"context"
	"errors"
)

// StartLimiter permits cover startup only; current-epoch execution slots
// govern steady state.
type StartLimiter struct {
	connector interface {
		RestoringConnector
		MaterializingConnector
		Cleaner
	}
	permits chan struct{}
}

func NewStartLimiter(connector interface {
	RestoringConnector
	MaterializingConnector
	Cleaner
}, maximum int) (*StartLimiter, error) {
	if connector == nil || maximum <= 0 {
		return nil, errors.New("VM start limiter requires a connector and positive maximum")
	}
	return &StartLimiter{connector: connector, permits: make(chan struct{}, maximum)}, nil
}

func (l *StartLimiter) withPermit(ctx context.Context, start func() (Session, error)) (Session, error) {
	select {
	case l.permits <- struct{}{}:
		defer func() { <-l.permits }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return start()
}

func (l *StartLimiter) Restore(ctx context.Context, request RestoreRequest) (Session, error) {
	return l.withPermit(ctx, func() (Session, error) { return l.connector.Restore(ctx, request) })
}

func (l *StartLimiter) Materialize(ctx context.Context, request MaterializeRequest) (Session, error) {
	return l.withPermit(ctx, func() (Session, error) { return l.connector.Materialize(ctx, request) })
}

func (l *StartLimiter) Cleanup(ctx context.Context, owner Owner) error {
	return l.connector.Cleanup(ctx, owner)
}
