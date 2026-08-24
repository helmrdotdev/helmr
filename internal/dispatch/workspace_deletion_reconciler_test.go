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

type workspaceDeletionFinalizerStub struct {
	limit   int32
	err     error
	invoked chan struct{}
}

func (s *workspaceDeletionFinalizerStub) FinalizeDeletingWorkspaces(
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

func TestPlacementReconcilerRunStartsWorkspaceDeleteLane(t *testing.T) {
	finalizer := &workspaceDeletionFinalizerStub{invoked: make(chan struct{}, 1)}
	reconciler := PlacementReconciler{
		workspaceExecDiscovery: workspaceExecRecoveryDiscovery{},
		workspaceExecAuthority: &workspaceExecRecoveryAuthority{},
		workspaceFinalizer:     finalizer,
		workspaceExecPolicy: placementLoopPolicy{
			interval: time.Hour, failureBackoff: time.Hour, timeout: time.Second, limit: 1,
		},
		workspaceDeletePolicy: placementLoopPolicy{
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
		t.Fatal("workspace delete lane was not invoked")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
}

func TestReconcileWorkspaceDeletesUsesBoundedFinalizer(t *testing.T) {
	finalizer := &workspaceDeletionFinalizerStub{}
	reconciler := PlacementReconciler{
		workspaceFinalizer: finalizer,
		workspaceDeletePolicy: placementLoopPolicy{
			limit: 17,
		},
	}
	if err := reconciler.ReconcileWorkspaceDeletes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if finalizer.limit != 17 {
		t.Fatalf("finalizer limit = %d, want 17", finalizer.limit)
	}

	want := errors.New("database unavailable")
	finalizer.err = want
	if err := reconciler.ReconcileWorkspaceDeletes(t.Context()); !errors.Is(err, want) {
		t.Fatalf("reconcile error = %v, want %v", err, want)
	}
}
