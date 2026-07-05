package cell

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
		CellID:             "cell-1",
		Component:          ComponentDispatcher,
		RequiredComponents: RoutingRequiredComponents(),
		Probe: func(context.Context) (db.CellHealthState, []byte, error) {
			return "", nil, errors.New("redis unavailable")
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if store.state != db.CellHealthStateUnavailable {
		t.Fatalf("state = %s, want unavailable", store.state)
	}
	if string(store.details) == "{}" {
		t.Fatalf("details = %s, want probe error details", store.details)
	}
}

type healthReportStore struct {
	cancel  context.CancelFunc
	state   db.CellHealthState
	details []byte
}

func (s *healthReportStore) UpsertCellComponentHealth(_ context.Context, arg db.UpsertCellComponentHealthParams) (db.CellComponentHealth, error) {
	s.state = arg.State
	s.details = arg.Details
	if s.cancel != nil {
		s.cancel()
	}
	return db.CellComponentHealth{}, nil
}

func (s *healthReportStore) RefreshCellHealthFromComponents(context.Context, db.RefreshCellHealthFromComponentsParams) (db.CellHealth, error) {
	return db.CellHealth{}, nil
}
