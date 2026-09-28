package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type computerDeletionFinalizerStub struct {
	limit   int32
	err     error
	invoked chan struct{}
}

func (s *computerDeletionFinalizerStub) FinalizeDeletingComputers(
	_ context.Context,
	limit int32,
) ([]pgtype.UUID, error) {
	s.limit = limit
	if s.invoked != nil {
		select {
		case s.invoked <- struct{}{}:
		default:
		}
	}
	return nil, s.err
}

func TestPlacementReconcilerRunStartsComputerDeleteLane(t *testing.T) {
	finalizer := &computerDeletionFinalizerStub{invoked: make(chan struct{}, 1)}
	reconciler := PlacementReconciler{
		computerCommandDiscovery: computerCommandRecoveryDiscovery{},
		computerCommandAuthority: &computerCommandRecoveryAuthority{},
		computerFinalizer:        finalizer,
		computerCommandPolicy: placementLoopPolicy{
			interval: time.Hour, failureBackoff: time.Hour, timeout: time.Second, limit: 1,
		},
		computerDeletePolicy: placementLoopPolicy{
			interval: time.Hour, failureBackoff: time.Hour, timeout: time.Second, limit: 1,
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	select {
	case <-finalizer.invoked:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("computer delete lane was not invoked")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
}

func TestReconcileComputerDeletesUsesBoundedFinalizer(t *testing.T) {
	finalizer := &computerDeletionFinalizerStub{}
	reconciler := PlacementReconciler{
		computerFinalizer: finalizer,
		computerDeletePolicy: placementLoopPolicy{
			limit: 17,
		},
	}
	if err := reconciler.ReconcileComputerDeletes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if finalizer.limit != 17 {
		t.Fatalf("finalizer limit = %d, want 17", finalizer.limit)
	}

	want := errors.New("database unavailable")
	finalizer.err = want
	if err := reconciler.ReconcileComputerDeletes(t.Context()); !errors.Is(err, want) {
		t.Fatalf("reconcile error = %v, want %v", err, want)
	}
}
