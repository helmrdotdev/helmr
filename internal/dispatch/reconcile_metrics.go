package dispatch

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type reconcileMetrics struct {
	cycles       metric.Int64Counter
	duration     metric.Float64Histogram
	runDecisions metric.Int64Counter
	runBatchSize metric.Int64Histogram
}

func newReconcileMetrics() reconcileMetrics {
	meter := otel.Meter("github.com/helmrdotdev/helmr/internal/dispatch")
	cycles, _ := meter.Int64Counter(
		"helmr.dispatch.reconcile.cycles",
		metric.WithDescription("Dispatcher reconciliation cycles by subsystem, work domain, and outcome."),
	)
	duration, _ := meter.Float64Histogram(
		"helmr.dispatch.reconcile.duration",
		metric.WithDescription("Dispatcher reconciliation cycle duration."),
		metric.WithUnit("s"),
	)
	runDecisions, _ := meter.Int64Counter(
		"helmr.dispatch.run.decisions",
		metric.WithDescription("Run placement decisions by classified outcome."),
	)
	runBatchSize, _ := meter.Int64Histogram(
		"helmr.dispatch.run.batch.attempts",
		metric.WithDescription("Candidates examined by each Run placement lane batch."),
	)
	return reconcileMetrics{
		cycles: cycles, duration: duration,
		runDecisions: runDecisions, runBatchSize: runBatchSize,
	}
}

func (m reconcileMetrics) observeRunBatch(ctx context.Context, batch runPlacementBatch) {
	if m.runBatchSize != nil {
		m.runBatchSize.Record(ctx, int64(batch.attempted))
	}
	if m.runDecisions == nil {
		return
	}
	outcomes := []struct {
		name  string
		count int
	}{
		{name: "placed", count: batch.placed},
		{name: "pending", count: batch.pending},
		{name: "changed", count: batch.changed},
		{name: "capacity_unavailable", count: batch.unavailable},
		{name: "failure", count: batch.attempted - batch.placed - batch.pending - batch.changed - batch.unavailable},
	}
	for _, outcome := range outcomes {
		if outcome.count == 0 {
			continue
		}
		m.runDecisions.Add(ctx, int64(outcome.count), metric.WithAttributes(
			attribute.String("helmr.dispatch.outcome", outcome.name),
		))
	}
}

func (m reconcileMetrics) observe(ctx context.Context, subsystem, domain, outcome string, elapsed time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String("helmr.dispatch.subsystem", subsystem),
		attribute.String("helmr.dispatch.domain", domain),
		attribute.String("helmr.dispatch.outcome", outcome),
	)
	if m.cycles != nil {
		m.cycles.Add(ctx, 1, attrs)
	}
	if m.duration != nil {
		m.duration.Record(ctx, elapsed.Seconds(), attrs)
	}
}
