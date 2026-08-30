package controlplane

import (
	"context"
	"errors"
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDiscoverWorkerRunLeasesReturnsOnlyExactWorkTuples(t *testing.T) {
	workerID := uuid.NewV7()
	firstLeaseID := uuid.NewV7()
	secondLeaseID := uuid.NewV7()
	store := &runLeaseDiscoveryStore{
		rows: []db.DiscoverWorkerRunLeaseWorkRow{
			{ID: pgvalue.UUID(firstLeaseID), LeaseSequence: 3},
			{ID: pgvalue.UUID(secondLeaseID), LeaseSequence: 7},
		},
	}

	response, err := discoverWorkerRunLeases(
		context.Background(),
		store,
		"run-workers",
		pgvalue.UUID(workerID),
		11,
	)
	if err != nil {
		t.Fatal(err)
	}
	if store.params.WorkerGroupID != "run-workers" ||
		store.params.WorkerInstanceID != pgvalue.UUID(workerID) ||
		store.params.WorkerEpoch != 11 ||
		store.params.RowLimit != workerRunLeaseDiscoveryLimit {
		t.Fatalf("discovery params = %+v", store.params)
	}
	if len(response.Items) != 2 ||
		response.Items[0].LeaseID != firstLeaseID.String() ||
		response.Items[0].LeaseSequence != 3 ||
		response.Items[1].LeaseID != secondLeaseID.String() ||
		response.Items[1].LeaseSequence != 7 {
		t.Fatalf("discovery response = %+v", response)
	}
}

func TestDiscoverWorkerRunLeasesReturnsAnEmptyList(t *testing.T) {
	response, err := discoverWorkerRunLeases(
		context.Background(),
		&runLeaseDiscoveryStore{},
		"run-workers",
		pgvalue.UUID(uuid.NewV7()),
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Items == nil || len(response.Items) != 0 {
		t.Fatalf("empty discovery response = %+v", response)
	}
}

func TestDiscoverWorkerRunLeasesPropagatesStorageFailure(t *testing.T) {
	expected := errors.New("database unavailable")
	_, err := discoverWorkerRunLeases(
		context.Background(),
		&runLeaseDiscoveryStore{err: expected},
		"run-workers",
		pgvalue.UUID(uuid.NewV7()),
		1,
	)
	if !errors.Is(err, expected) {
		t.Fatalf("discovery error = %v, want %v", err, expected)
	}
}

type runLeaseDiscoveryStore struct {
	db.Querier
	rows   []db.DiscoverWorkerRunLeaseWorkRow
	err    error
	params db.DiscoverWorkerRunLeaseWorkParams
}

func (s *runLeaseDiscoveryStore) DiscoverWorkerRunLeaseWork(
	_ context.Context,
	params db.DiscoverWorkerRunLeaseWorkParams,
) ([]db.DiscoverWorkerRunLeaseWorkRow, error) {
	s.params = params
	return s.rows, s.err
}
