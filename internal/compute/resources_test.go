package compute

import "testing"

func TestResourceVectorFits(t *testing.T) {
	capacity := ResourceVector{
		MilliCPU:  4000,
		MemoryMiB: 8192,
		DiskMiB:   32768,
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
	if ImageBuildGuestPIDsMax != 1024 {
		t.Fatalf("image-build guest pids.max = %d, want 1024", ImageBuildGuestPIDsMax)
	}
	if got, want := BuildGuestResources(), (ResourceVector{MilliCPU: 2000, MemoryMiB: 2048, DiskMiB: 32768, Slots: 1}); got != want {
		t.Fatalf("build guest = %+v, want %+v", got, want)
	}
	if got, want := BuildEnvelopeResources(), (ResourceVector{MilliCPU: 3000, MemoryMiB: 4096, DiskMiB: 32768, Slots: 1}); got != want {
		t.Fatalf("build envelope = %+v, want %+v", got, want)
	}
	if got, want := ImageBuildGuestResources(), BuildEnvelopeResources(); got != want {
		t.Fatalf("ImageBuildGuestResources = %+v, want %+v", got, want)
	}
}
