package workergroup

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestRunHealthReporterReportsProbeFailureUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &healthReportStore{cancel: cancel}

	err := RunHealthReporter(ctx, store, HealthReporterConfig{
		WorkerGroupID:      "worker-group-1",
		Component:          ComponentDispatcher,
		RequiredComponents: RoutingRequiredComponents(),
		Probe: func(context.Context) (db.WorkerGroupHealthState, []byte, error) {
			return "", nil, errors.New("redis unavailable")
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if store.state != db.WorkerGroupHealthStateUnavailable {
		t.Fatalf("state = %s, want unavailable", store.state)
	}
	if string(store.details) == "{}" {
		t.Fatalf("details = %s, want probe error details", store.details)
	}
}

type healthReportStore struct {
	cancel  context.CancelFunc
	state   db.WorkerGroupHealthState
	details []byte
}

func (s *healthReportStore) ReportWorkerGroupHealth(_ context.Context, arg db.ReportWorkerGroupHealthParams) (db.WorkerGroup, error) {
	s.state = arg.HealthState
	s.details = arg.HealthDetails
	if s.cancel != nil {
		s.cancel()
	}
	return db.WorkerGroup{}, nil
}
