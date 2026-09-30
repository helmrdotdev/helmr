package computer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
	metrics        deletionMetrics
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
		timeout: deletionFinalizeTimeout, limit: deletionFinalizeLimit, metrics: newDeletionMetrics(), log: log,
	}, nil
}

// Run finalizes a bounded batch each cycle until ctx ends, and returns the
// context's error.
func (r *DeletionReconciler) Run(ctx context.Context) error {
	for {
		delay, ok := r.cycle(ctx)
		if !ok {
			return ctx.Err()
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

// cycle finalizes one batch under the cycle timeout and records its outcome.
// It returns the delay before the next cycle, or false once ctx has ended.
func (r *DeletionReconciler) cycle(ctx context.Context) (time.Duration, bool) {
	started := time.Now()
	cycleCtx, cancel := context.WithTimeout(ctx, r.timeout)
	err := r.finalizeDeleting(cycleCtx)
	cancel()
	if ctx.Err() != nil {
		return 0, false
	}
	outcome := "success"
	delay := r.interval
	if err != nil && !errors.Is(err, context.Canceled) {
		outcome = "failure"
		delay = r.failureBackoff
		r.log.Warn("Computer deletion finalization failed", "duration_ms", time.Since(started).Milliseconds(), "error", err)
	}
	r.metrics.observe(ctx, outcome, time.Since(started))
	return delay, true
}

// finalizeDeleting finalizes one bounded batch of deleting Computers.
func (r *DeletionReconciler) finalizeDeleting(ctx context.Context) error {
	if _, err := r.finalizer.FinalizeDeletingComputers(ctx, r.limit); err != nil {
		return fmt.Errorf("finalize deleting computers: %w", err)
	}
	return nil
}

// deletionMetrics records each deletion finalization cycle by outcome.
type deletionMetrics struct {
	cycles   metric.Int64Counter
	duration metric.Float64Histogram
}

func newDeletionMetrics() deletionMetrics {
	meter := otel.Meter("github.com/helmrdotdev/helmr/internal/computer")
	cycles, _ := meter.Int64Counter(
		"helmr.computer.deletion.cycles",
		metric.WithDescription("Computer deletion finalization cycles by outcome."),
	)
	duration, _ := meter.Float64Histogram(
		"helmr.computer.deletion.duration",
		metric.WithDescription("Computer deletion finalization cycle duration."),
		metric.WithUnit("s"),
	)
	return deletionMetrics{cycles: cycles, duration: duration}
}

func (m deletionMetrics) observe(ctx context.Context, outcome string, elapsed time.Duration) {
	attrs := metric.WithAttributes(attribute.String("helmr.computer.outcome", outcome))
	if m.cycles != nil {
		m.cycles.Add(ctx, 1, attrs)
	}
	if m.duration != nil {
		m.duration.Record(ctx, elapsed.Seconds(), attrs)
	}
}
