package dispatch

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type reconcileMetrics struct {
	cycles   metric.Int64Counter
	duration metric.Float64Histogram
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
	return reconcileMetrics{cycles: cycles, duration: duration}
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
