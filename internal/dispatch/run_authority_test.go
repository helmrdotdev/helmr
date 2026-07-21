package dispatch

import (
	"math"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestNormalizeRunResourcesCopiesWorkspaceAuthority(t *testing.T) {
	resources, err := normalizeRunResources(deployment.ResourcesManifest{
		MilliCPU:  1500,
		MemoryMiB: 2048,
		DiskMiB:   4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resources.cpuMillis != 1500 ||
		resources.memoryBytes != 2048*mebibyte ||
		resources.workloadDisk != 4096*mebibyte ||
		resources.scratchBytes != 0 ||
		resources.executionSlots != 1 {
		t.Fatalf("normalized resources = %+v", resources)
	}
}

func TestNormalizeRunResourcesRejectsInvalidWorkspaceAuthority(t *testing.T) {
	for _, resources := range []deployment.ResourcesManifest{
		{MilliCPU: 0, MemoryMiB: 1, DiskMiB: 1},
		{MilliCPU: 1, MemoryMiB: 0, DiskMiB: 1},
		{MilliCPU: 1, MemoryMiB: 1, DiskMiB: 0},
		{MilliCPU: 1, MemoryMiB: math.MaxInt64, DiskMiB: 1},
		{MilliCPU: 1, MemoryMiB: 1, DiskMiB: math.MaxInt64},
	} {
		if normalized, err := normalizeRunResources(resources); err == nil {
			t.Fatalf("normalizeRunResources(%+v) = %+v, want error", resources, normalized)
		}
	}
}
