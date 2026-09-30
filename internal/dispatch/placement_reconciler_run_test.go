package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

type invokedCommandDiscovery struct {
	computerCommandRecoveryDiscovery
	invoked chan struct{}
}

func (d invokedCommandDiscovery) ListRecoverableComputerCommandCandidates(
	ctx context.Context,
	limit int32,
) ([]db.ListRecoverableComputerCommandCandidatesRow, error) {
	select {
	case d.invoked <- struct{}{}:
	default:
	}
	return d.computerCommandRecoveryDiscovery.ListRecoverableComputerCommandCandidates(ctx, limit)
}

// Run starts the Computer Command lane and joins every loop once cancelled.
func TestPlacementReconcilerRunJoinsLoopsOnCancellation(t *testing.T) {
	discovery := invokedCommandDiscovery{invoked: make(chan struct{}, 1)}
	reconciler := PlacementReconciler{
		computerCommandDiscovery: discovery,
		computerCommandAuthority: &computerCommandRecoveryAuthority{},
		computerCommandPolicy: placementLoopPolicy{
			interval: time.Hour, failureBackoff: time.Hour, timeout: time.Second, limit: 1,
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	select {
	case <-discovery.invoked:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("computer command lane was not invoked")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
}
