package workergroup

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type demandStoreStub struct {
	rows []db.ListWorkerDemandObservationsRow
}

func (stub demandStoreStub) ListWorkerDemandObservations(context.Context) ([]db.ListWorkerDemandObservationsRow, error) {
	return stub.rows, nil
}

func TestObserveDemandAggregatesQueuedResources(t *testing.T) {
	observedAt := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	observations, err := ObserveDemand(context.Background(), demandStoreStub{rows: []db.ListWorkerDemandObservationsRow{{
		WorkerGroupID:                         "worker-group-1",
		RegionID:                              "us-east-1",
		State:                                 "active",
		AllowsRun:                             true,
		AllowsBuild:                           true,
		QueuedRuns:                            3,
		QueuedRunCpuMillis:                    750,
		QueuedRunMemoryMib:                    6,
		QueuedBuilds:                          2,
		ReadyRunWorkers:                       1,
		RunAvailableCpuMillis:                 500,
		RunAvailableMemoryBytes:               4096,
		RunAvailableGuestEphemeralDiskBytes:   8192,
		RunAvailableVmSlots:                   4,
		RunAvailableConsumers:                 2,
		ReadyBuildWorkers:                     1,
		BuildAvailableCpuMillis:               750,
		BuildAvailableMemoryBytes:             6144,
		BuildAvailableGuestEphemeralDiskBytes: 12288,
		BuildAvailableExecutors:               3,
		RegisteringWorkers:                    2,
		DrainingWorkers:                       1,
		ObservedAt:                            pgtype.Timestamptz{Time: observedAt, Valid: true},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Fatalf("observation count = %d, want 1", len(observations))
	}
	got := observations[0]
	if got.Run == nil || got.Run.QueuedResources.CPUMillis != 750 || got.Run.QueuedResources.VMSlots != 3 {
		t.Fatalf("Run demand = %#v", got.Run)
	}
	if got.Build == nil || got.Build.QueuedResources.MemoryBytes != 2*compute.BuildEnvelopeResources().MemoryMiB*mebibyte || got.Build.QueuedResources.BuildExecutors != 2 {
		t.Fatalf("Build demand = %#v", got.Build)
	}
	if got.Run.AvailableCapacity.RunConsumers != 2 || got.Build.AvailableCapacity.BuildExecutors != 3 {
		t.Fatalf("available capacity = Run %#v Build %#v", got.Run.AvailableCapacity, got.Build.AvailableCapacity)
	}
	if !got.ObservedAt.Equal(observedAt) || got.RegisteringWorkers != 2 || got.DrainingWorkers != 1 {
		t.Fatalf("observation metadata = %#v", got)
	}
}

func TestObserveDemandRejectsAggregateOverflow(t *testing.T) {
	_, err := ObserveDemand(context.Background(), demandStoreStub{rows: []db.ListWorkerDemandObservationsRow{{
		WorkerGroupID:      "worker-group-1",
		AllowsRun:          true,
		QueuedRuns:         1,
		QueuedRunCpuMillis: 1,
		QueuedRunMemoryMib: math.MaxInt64,
		ObservedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}}})
	if err == nil {
		t.Fatal("expected overflow error")
	}
}
