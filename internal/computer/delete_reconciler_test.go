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
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
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

func TestDeletionFinalizationUsesBoundedFinalizer(t *testing.T) {
	finalizer := &deletionFinalizerStub{}
	reconciler := DeletionReconciler{finalizer: finalizer, limit: 17}
	if err := reconciler.finalizeDeleting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if finalizer.limit != 17 {
		t.Fatalf("finalizer limit = %d, want 17", finalizer.limit)
	}

	want := errors.New("database unavailable")
	finalizer.err = want
	if err := reconciler.finalizeDeleting(t.Context()); !errors.Is(err, want) {
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

// deletionCycleCounter and deletionCycleDuration record the outcome
// attribute of each cycle.
type deletionCycleCounter struct {
	noop.Int64Counter
	outcomes []string
}

func (c *deletionCycleCounter) Add(_ context.Context, increment int64, options ...metric.AddOption) {
	attrs := metric.NewAddConfig(options).Attributes()
	value, _ := attrs.Value("helmr.computer.outcome")
	for range increment {
		c.outcomes = append(c.outcomes, value.AsString())
	}
}

type deletionCycleDuration struct {
	noop.Float64Histogram
	outcomes []string
}

func (h *deletionCycleDuration) Record(_ context.Context, _ float64, options ...metric.RecordOption) {
	attrs := metric.NewRecordConfig(options).Attributes()
	value, _ := attrs.Value("helmr.computer.outcome")
	h.outcomes = append(h.outcomes, value.AsString())
}

// Each cycle records its count and duration by outcome, and a failure backs
// off instead of waiting the interval.
func TestDeletionReconcilerRecordsCycleOutcomes(t *testing.T) {
	cycles, durations := &deletionCycleCounter{}, &deletionCycleDuration{}
	finalizer := &deletionFinalizerStub{}
	reconciler := DeletionReconciler{
		finalizer: finalizer, interval: time.Second, failureBackoff: time.Minute, timeout: time.Second, limit: 1,
		metrics: deletionMetrics{cycles: cycles, duration: durations},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if delay, ok := reconciler.cycle(t.Context()); !ok || delay != time.Second {
		t.Fatalf("success cycle = %v %v", delay, ok)
	}
	finalizer.err = errors.New("database unavailable")
	if delay, ok := reconciler.cycle(t.Context()); !ok || delay != time.Minute {
		t.Fatalf("failure cycle = %v %v", delay, ok)
	}
	want := []string{"success", "failure"}
	for name, got := range map[string][]string{"cycles": cycles.outcomes, "durations": durations.outcomes} {
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s outcomes = %v, want %v", name, got, want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, ok := reconciler.cycle(ctx); ok || len(cycles.outcomes) != 2 || len(durations.outcomes) != 2 {
		t.Fatalf("cancelled cycle recorded: %v %v", ok, cycles.outcomes)
	}
}
