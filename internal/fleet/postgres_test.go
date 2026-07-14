package fleet

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestTerminationCandidatePrefersReadyDisabledThenOldestDrain(t *testing.T) {
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	workers := []Worker{
		{ID: "drain-new", State: WorkerDraining},
		{ID: "disabled-ready", State: WorkerDisabled, LocalCleanupComplete: true},
		{ID: "drain-old", State: WorkerDraining},
	}
	drains := map[string]time.Time{"drain-new": now.Add(-time.Minute), "drain-old": now.Add(-time.Hour)}
	if got := selectTerminationCandidate(workers, drains); got != "disabled-ready" {
		t.Fatalf("candidate = %q, want ready disabled", got)
	}
	workers[1].LocalCleanupComplete = false
	if got := selectTerminationCandidate(workers, drains); got != "drain-old" {
		t.Fatalf("candidate = %q, want oldest drain", got)
	}
}

func TestQueuedRunDemandUsesConfiguredScratchPartition(t *testing.T) {
	bucket, err := runDemandBucket(db.ListFleetRunDemandRow{
		DemandState: "queued", CompatibilityKey: "run-workers", MilliCpu: 1000,
		MemoryBytes: 1024, WorkloadDiskBytes: 2048, VmSlots: 1, DemandCount: 3,
	}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.Shape.ScratchBytes != 4096 || bucket.Count != 3 {
		t.Fatalf("bucket = %#v", bucket)
	}
}

func TestActiveRunDemandKeepsLeaseScratchPartition(t *testing.T) {
	bucket, err := runDemandBucket(db.ListFleetRunDemandRow{
		DemandState: "active", CompatibilityKey: "run-workers", MilliCpu: 1000,
		MemoryBytes: 1024, WorkloadDiskBytes: 2048, ScratchBytes: 8192, VmSlots: 1, DemandCount: 1,
	}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.Shape.ScratchBytes != 8192 {
		t.Fatalf("bucket = %#v", bucket)
	}
}
