package workerhost

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
)

func TestCapacityAllocatorReserveAndRelease(t *testing.T) {
	allocator, err := NewCapacityAllocator(compute.ResourceVector{
		MilliCPU:  4000,
		MemoryMiB: 8192,
		DiskMiB:   20480,
		Slots:     4,
	})
	if err != nil {
		t.Fatal(err)
	}

	available, err := allocator.Reserve(Reservation{
		ID:        "lease-1",
		Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 2048, DiskMiB: 4096, Slots: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if available != (compute.ResourceVector{MilliCPU: 3000, MemoryMiB: 6144, DiskMiB: 16384, Slots: 3}) {
		t.Fatalf("available after reserve = %+v", available)
	}

	available, err = allocator.Release("lease-1")
	if err != nil {
		t.Fatal(err)
	}
	if available != (compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 8192, DiskMiB: 20480, Slots: 4}) {
		t.Fatalf("available after release = %+v", available)
	}
}

func TestCapacityAllocatorRejectsOvercommit(t *testing.T) {
	allocator, err := NewCapacityAllocator(compute.ResourceVector{
		MilliCPU:  2000,
		MemoryMiB: 2048,
		DiskMiB:   1024,
		Slots:     1,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = allocator.Reserve(Reservation{
		ID:        "lease-1",
		Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 512, Slots: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = allocator.Reserve(Reservation{
		ID:        "lease-2",
		Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 512, Slots: 1},
	})
	if !errors.Is(err, compute.ErrNoCapacity) {
		t.Fatalf("second reservation error = %v, want no capacity", err)
	}
}

func TestCapacityAllocatorRejectsDuplicateAndMissingReservations(t *testing.T) {
	allocator, err := NewCapacityAllocator(compute.ResourceVector{
		MilliCPU:  2000,
		MemoryMiB: 2048,
		Slots:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation := Reservation{
		ID:        "lease-1",
		Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
	}
	if _, err := allocator.Reserve(reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Reserve(reservation); !errors.Is(err, ErrReservationExists) {
		t.Fatalf("duplicate reservation error = %v, want exists", err)
	}
	if _, err := allocator.Release("missing"); !errors.Is(err, ErrReservationMissing) {
		t.Fatalf("missing reservation error = %v, want missing", err)
	}
}
