package computer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	deletionFinalizeInterval       = time.Second
	deletionFinalizeFailureBackoff = time.Second
	deletionFinalizeTimeout        = 15 * time.Second
	deletionFinalizeLimit          = int32(32)
)

type deletionFinalizer interface {
	FinalizeDeletingComputers(context.Context, int32) ([]pgtype.UUID, error)
}

// DeletionReconciler repeatedly finalizes deleting Computers once nothing
// holds them any longer.
type DeletionReconciler struct {
	finalizer      deletionFinalizer
	interval       time.Duration
	failureBackoff time.Duration
	timeout        time.Duration
	limit          int32
	log            *slog.Logger
}

// NewDeletionReconciler finalizes deletions through database.
func NewDeletionReconciler(database db.DBTX, log *slog.Logger) (*DeletionReconciler, error) {
	if database == nil {
		return nil, errors.New("computer deletion reconciler requires a database")
	}
	if log == nil {
		log = slog.Default()
	}
	return &DeletionReconciler{
		finalizer: db.New(database), interval: deletionFinalizeInterval, failureBackoff: deletionFinalizeFailureBackoff,
		timeout: deletionFinalizeTimeout, limit: deletionFinalizeLimit, log: log,
	}, nil
}

// Run finalizes a bounded batch each cycle until ctx ends, and returns the
// context's error.
func (r *DeletionReconciler) Run(ctx context.Context) error {
	for {
		started := time.Now()
		cycleCtx, cancel := context.WithTimeout(ctx, r.timeout)
		err := r.FinalizeDeleting(cycleCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := r.interval
		if err != nil && !errors.Is(err, context.Canceled) {
			delay = r.failureBackoff
			r.log.Warn("Computer deletion finalization failed", "duration_ms", time.Since(started).Milliseconds(), "error", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// FinalizeDeleting finalizes one bounded batch of deleting Computers.
func (r *DeletionReconciler) FinalizeDeleting(ctx context.Context) error {
	if _, err := r.finalizer.FinalizeDeletingComputers(ctx, r.limit); err != nil {
		return fmt.Errorf("finalize deleting computers: %w", err)
	}
	return nil
}
