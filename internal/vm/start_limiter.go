package vm

import (
	"context"
	"errors"
)

// Permits cover startup only; certified execution slots govern steady state.
type StartLimiter struct {
	connector interface {
		Connector
		RestoringConnector
		MaterializingConnector
		RuntimeCleanupConnector
	}
	permits chan struct{}
}

func NewStartLimiter(connector interface {
	Connector
	RestoringConnector
	MaterializingConnector
	RuntimeCleanupConnector
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

func (l *StartLimiter) Connect(ctx context.Context, request ConnectRequest) (Session, error) {
	return l.withPermit(ctx, func() (Session, error) { return l.connector.Connect(ctx, request) })
}

func (l *StartLimiter) Restore(ctx context.Context, request RestoreRequest) (Session, error) {
	return l.withPermit(ctx, func() (Session, error) { return l.connector.Restore(ctx, request) })
}

func (l *StartLimiter) Materialize(ctx context.Context, request MaterializeRequest) (Session, error) {
	return l.withPermit(ctx, func() (Session, error) { return l.connector.Materialize(ctx, request) })
}

func (l *StartLimiter) CleanupRuntime(ctx context.Context, runtimeID string) error {
	return l.connector.CleanupRuntime(ctx, runtimeID)
}
