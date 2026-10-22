package reservation

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOptionalAdoptionIsSingleAssignmentWithoutFreeCapacity(t *testing.T) {
	shape := Vector{CPUMillis: 1, MemoryBytes: 1, HostDiskBytes: 3, VMSlots: 1}
	l := newTestLedger(t, shape)
	from := Key{Kind: "leaf", ID: "physical", Epoch: 1}
	if _, err := l.ReserveOptional(from, shape, shape, Vector{}); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	var winners atomic.Int64
	for i := range 50 {
		jobs.Go(func() {
			if err := l.AdoptOptional(from, Key{Kind: "instance", ID: fmt.Sprint(i), Epoch: 2}, Vector{CPUMillis: 1, MemoryBytes: 1, HostDiskBytes: 2, VMSlots: 1}); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrInvalidKey) {
				t.Error(err)
			}
			if l.Snapshot().Used != shape {
				t.Error("adoption released physical capacity")
			}
		})
	}
	jobs.Wait()
	if got := l.Snapshot(); winners.Load() != 1 || len(got.Optional) != 0 || len(got.Reservations) != 1 || got.Used != shape {
		t.Fatal(winners.Load(), got)
	}
}

func TestOptionalReservationsPreserveHeadroomForRealWork(t *testing.T) {
	shape := Vector{CPUMillis: 1, MemoryBytes: 1, HostDiskBytes: 1, VMSlots: 1}
	capacity := Vector{CPUMillis: 2, MemoryBytes: 2, HostDiskBytes: 2, VMSlots: 2}
	l := newTestLedger(t, capacity)
	optional := Key{Kind: "instance", Epoch: 1, ID: "seed"}
	if _, err := l.ReserveOptional(optional, shape, capacity, shape); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveOptional(Key{Kind: "leaf", Epoch: 1, ID: "next"}, shape, capacity, shape); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("optional borrowed headroom: %v", err)
	}
	if _, err := l.Reserve(Key{Kind: "instance", Epoch: 1, ID: "real"}, shape); err != nil {
		t.Fatalf("real work could not use headroom: %v", err)
	}
	if got := l.Snapshot(); got.Used != capacity || len(got.Optional) != 1 {
		t.Fatalf("physical accounting: %+v", got)
	}
	// A reduced ceiling never drops the old physical charge.
	if _, err := l.ReserveOptional(Key{Kind: "leaf", Epoch: 1, ID: "next"}, shape, Vector{}, Vector{}); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("zero optional policy: %v", err)
	}
	if l.Snapshot().Used != capacity {
		t.Fatal("policy decrease released physical charge")
	}
	if err := l.Release(optional); err != nil {
		t.Fatal(err)
	}
	if got := l.Snapshot(); got.Used != shape || len(got.Optional) != 0 {
		t.Fatalf("release: %+v", got)
	}
}

func TestOptionalReservationsCannotRacePastCombinedCeiling(t *testing.T) {
	l := newTestLedger(t, Vector{CPUMillis: 100, MemoryBytes: 100, HostDiskBytes: 100, VMSlots: 100})
	shape := Vector{CPUMillis: 1, MemoryBytes: 1, HostDiskBytes: 1, VMSlots: 1}
	ceiling := Vector{CPUMillis: 10, MemoryBytes: 10, HostDiskBytes: 10, VMSlots: 10}
	var jobs sync.WaitGroup
	var admitted atomic.Int64
	for i := range 100 {
		jobs.Go(func() {
			created, err := l.ReserveOptional(Key{Kind: "leaf", Epoch: 1, ID: fmt.Sprint(i)}, shape, ceiling, Vector{})
			if err != nil && !errors.Is(err, ErrCapacityExceeded) {
				t.Error(err)
			}
			if created {
				admitted.Add(1)
			}
		})
	}
	jobs.Wait()
	if admitted.Load() != 10 || l.Snapshot().Used != ceiling {
		t.Fatalf("overcommitted combined budget: %+v", l.Snapshot())
	}
}

func TestOptionalReservationClassCannotChangeByReplay(t *testing.T) {
	l := newTestLedger(t, Vector{CPUMillis: 2, MemoryBytes: 2, HostDiskBytes: 4, VMSlots: 2})
	key := Key{Kind: "instance", Epoch: 1, ID: "seed"}
	shape := Vector{CPUMillis: 1, MemoryBytes: 1, HostDiskBytes: 2, VMSlots: 1}
	if _, err := l.ReserveOptional(key, shape, shape, Vector{}); err != nil {
		t.Fatal(err)
	}
	if err := l.ReleaseHostDisk(key, 1); err != nil {
		t.Fatal(err)
	}
	if l.Snapshot().Optional[key].HostDiskBytes != 1 {
		t.Fatal("optional accounting retained released disk")
	}
	if err := l.Release(key); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(key, shape); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveOptional(key, shape, shape, Vector{}); !errors.Is(err, ErrDuplicateReservation) {
		t.Fatalf("reclassified real reservation: %v", err)
	}
}

func TestOptionalResizeChecksGrowthAndRetainsChargeOnFailure(t *testing.T) {
	l := newTestLedger(t, Vector{CPUMillis: 1, MemoryBytes: 1, HostDiskBytes: 10, VMSlots: 1})
	key := Key{Kind: "cache", Epoch: 1, ID: "objects"}
	ceiling := Vector{HostDiskBytes: 8}
	if _, err := l.ReserveOptional(key, Vector{HostDiskBytes: 4}, ceiling, Vector{}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(Key{Kind: "instance", Epoch: 1, ID: "real"}, Vector{HostDiskBytes: 3}); err != nil {
		t.Fatal(err)
	}
	for _, limits := range []struct{ ceiling, headroom Vector }{
		{Vector{HostDiskBytes: 5}, Vector{}},
		{ceiling, Vector{HostDiskBytes: 2}},
	} {
		if err := l.ResizeOptional(key, Vector{HostDiskBytes: 6}, limits.ceiling, limits.headroom); !errors.Is(err, ErrCapacityExceeded) {
			t.Fatalf("growth exceeded bounds: %v", err)
		}
		if got := l.Snapshot(); got.Used.HostDiskBytes != 7 || got.Optional[key].HostDiskBytes != 4 {
			t.Fatalf("failed growth changed physical charge: %+v", got)
		}
	}
	if err := l.ResizeOptional(key, Vector{HostDiskBytes: 6}, ceiling, Vector{HostDiskBytes: 1}); err != nil {
		t.Fatal(err)
	}
	// Physical shrink must remain possible after the policy is reduced to zero.
	if err := l.ResizeOptional(key, Vector{HostDiskBytes: 2}, Vector{}, Vector{}); err != nil {
		t.Fatal(err)
	}
	if got := l.Snapshot(); got.Used.HostDiskBytes != 5 || got.Optional[key].HostDiskBytes != 2 {
		t.Fatalf("shrink accounting: %+v", got)
	}
}
