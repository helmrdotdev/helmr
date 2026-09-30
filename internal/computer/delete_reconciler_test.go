package computer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

// nilDBTX is a database the constructor accepts; the tests never query it.
type nilDBTX struct{ db.DBTX }

type deletionFinalizerStub struct {
	limit   int32
	err     error
	invoked chan struct{}
}

func (s *deletionFinalizerStub) FinalizeDeletingComputers(_ context.Context, limit int32) ([]pgtype.UUID, error) {
	s.limit = limit
	if s.invoked != nil {
		select {
		case s.invoked <- struct{}{}:
		default:
		}
	}
	return nil, s.err
}

func TestDeletionReconcilerRunFinalizesUntilCancelled(t *testing.T) {
	finalizer := &deletionFinalizerStub{invoked: make(chan struct{}, 1)}
	reconciler := DeletionReconciler{
		finalizer: finalizer, interval: time.Hour, failureBackoff: time.Hour, timeout: time.Second, limit: 1,
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
		t.Fatal("deletion finalizer was not invoked")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
}

func TestFinalizeDeletingUsesBoundedFinalizer(t *testing.T) {
	finalizer := &deletionFinalizerStub{}
	reconciler := DeletionReconciler{finalizer: finalizer, limit: 17}
	if err := reconciler.FinalizeDeleting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if finalizer.limit != 17 {
		t.Fatalf("finalizer limit = %d, want 17", finalizer.limit)
	}

	want := errors.New("database unavailable")
	finalizer.err = want
	if err := reconciler.FinalizeDeleting(t.Context()); !errors.Is(err, want) {
		t.Fatalf("finalize error = %v, want %v", err, want)
	}
}

func TestNewDeletionReconcilerKeepsDispatchBatch(t *testing.T) {
	if _, err := NewDeletionReconciler(nil, nil); err == nil {
		t.Fatal("reconciler without database accepted")
	}
	reconciler, err := NewDeletionReconciler(nilDBTX{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reconciler.interval != time.Second || reconciler.failureBackoff != time.Second || reconciler.timeout != 15*time.Second || reconciler.limit != 32 {
		t.Fatalf("deletion cadence = %+v", reconciler)
	}
}
