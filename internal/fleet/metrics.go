package fleet

import (
	"context"
	"time"
)

type Outcome string

const (
	OutcomePlanned   Outcome = "planned"
	OutcomeConfirmed Outcome = "confirmed"
	OutcomeRetrying  Outcome = "retrying"
)

// Publisher failures never affect authoritative reconciliation.
type FleetMetrics struct {
	GroupID                          string
	UncappedRequired                 int
	UnmetDeficit                     int
	CapReason                        CapReason
	Desired                          int
	Supply                           int
	Pending                          int
	Billable                         int
	UncertifiedRunLaunchAttestations int
	BootstrapPending                 bool
	Action                           Action
	Outcome                          Outcome
	DrainAge                         time.Duration
	QueueAge                         time.Duration
	DrainTimedOut                    bool
}

type MetricsPublisher interface {
	Publish(context.Context, FleetMetrics) error
}

type discardMetrics struct{}

func (discardMetrics) Publish(context.Context, FleetMetrics) error { return nil }
