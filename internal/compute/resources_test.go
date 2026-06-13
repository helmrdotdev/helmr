package compute

import "testing"

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
