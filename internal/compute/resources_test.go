package compute

import (
	"errors"
	"testing"
)

func TestResourceVectorFits(t *testing.T) {
	capacity := ResourceVector{
		MilliCPU:  4000,
		MemoryMiB: 8192,
		DiskMiB:   20480,
		Slots:     4,
	}

	if !capacity.Fits(ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 2}) {
		t.Fatal("expected capacity to satisfy smaller request")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 5000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 1}) {
		t.Fatal("expected CPU overcommit to fail")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 1000, MemoryMiB: 16384, DiskMiB: 1024, Slots: 1}) {
		t.Fatal("expected memory overcommit to fail")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 1000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 5}) {
		t.Fatal("expected slot overcommit to fail")
	}
}

func TestBuildResourceContracts(t *testing.T) {
	if BuildGuestPIDsMax != 1024 {
		t.Fatalf("build guest pids.max = %d, want 1024", BuildGuestPIDsMax)
	}
	if got, want := BuildGuestResources(), (ResourceVector{MilliCPU: 2000, MemoryMiB: 2048, DiskMiB: 20480, Slots: 1}); got != want {
		t.Fatalf("build guest = %+v, want %+v", got, want)
	}
	if got, want := ManagerAcquireResources(), (ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1}); got != want {
		t.Fatalf("manager acquisition guest = %+v, want %+v", got, want)
	}
	if got, want := BuildEnvelopeResources(), (ResourceVector{MilliCPU: 3000, MemoryMiB: 4096, DiskMiB: 32768, Slots: 1}); got != want {
		t.Fatalf("build envelope = %+v, want %+v", got, want)
	}
}

func TestBuildAllocatableResourcesSubtractsFixedServiceReserve(t *testing.T) {
	got, err := BuildAllocatableResources(ResourceVector{
		MilliCPU:  4000,
		MemoryMiB: 8192,
		Slots:     1,
	})
	if err != nil {
		t.Fatalf("BuildAllocatableResources(): %v", err)
	}
	want := ResourceVector{MilliCPU: 3000, MemoryMiB: 6144, Slots: 1}
	if got != want {
		t.Fatalf("allocatable = %+v, want %+v", got, want)
	}
}

func TestBuildAllocatableResourcesRequiresCapacityBeyondReserve(t *testing.T) {
	tests := []ResourceVector{
		{MilliCPU: 1000, MemoryMiB: 4096, Slots: 1},
		{MilliCPU: 4000, MemoryMiB: 2048, Slots: 1},
	}
	for _, hostPool := range tests {
		if _, err := BuildAllocatableResources(hostPool); !errors.Is(err, ErrNoCapacity) {
			t.Fatalf("BuildAllocatableResources(%+v) error = %v, want %v", hostPool, err, ErrNoCapacity)
		}
	}
}
