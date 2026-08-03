package capacity

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNewValidatesCapacity(t *testing.T) {
	valid := Vector{
		CPUMillis:               1,
		MemoryBytes:             1,
		GuestEphemeralDiskBytes: 1,
		VMSlots:                 1,
		BuildSlots:              1,
	}
	tests := []struct {
		name   string
		mutate func(*Vector)
	}{
		{name: "zero cpu", mutate: func(vector *Vector) { vector.CPUMillis = 0 }},
		{name: "negative cpu", mutate: func(vector *Vector) { vector.CPUMillis = -1 }},
		{name: "zero memory", mutate: func(vector *Vector) { vector.MemoryBytes = 0 }},
		{name: "negative memory", mutate: func(vector *Vector) { vector.MemoryBytes = -1 }},
		{name: "negative guest ephemeral disk", mutate: func(vector *Vector) { vector.GuestEphemeralDiskBytes = -1 }},
		{name: "negative VM slots", mutate: func(vector *Vector) { vector.VMSlots = -1 }},
		{name: "negative build slots", mutate: func(vector *Vector) { vector.BuildSlots = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capacity := valid
			test.mutate(&capacity)
			if _, err := New(capacity); !errors.Is(err, ErrInvalidCapacity) {
				t.Fatalf("New() error = %v, want %v", err, ErrInvalidCapacity)
			}
		})
	}

	valid.GuestEphemeralDiskBytes = 0
	valid.VMSlots = 0
	valid.BuildSlots = 0
	if _, err := New(valid); err != nil {
		t.Fatalf("New() with optional zero dimensions: %v", err)
	}
}

func TestReserveValidatesKeyAndRequest(t *testing.T) {
	ledger := newTestLedger(t, Vector{
		CPUMillis: 10, MemoryBytes: 10, GuestEphemeralDiskBytes: 10,
		VMSlots: 10, BuildSlots: 10,
	})
	validKey := Key{Kind: "run", Epoch: 1, ID: "run-1"}
	keyTests := []Key{
		{Epoch: 1, ID: "run-1"},
		{Kind: " ", Epoch: 1, ID: "run-1"},
		{Kind: " run", Epoch: 1, ID: "run-1"},
		{Kind: "run", ID: "run-1"},
		{Kind: "run", Epoch: -1, ID: "run-1"},
		{Kind: "run", Epoch: 1},
		{Kind: "run", Epoch: 1, ID: " "},
		{Kind: "run", Epoch: 1, ID: "run-1 "},
	}
	for _, key := range keyTests {
		if created, err := ledger.Reserve(key, Vector{CPUMillis: 1}); created || !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Reserve(%+v) error = %v, want %v", key, err, ErrInvalidKey)
		}
		if err := ledger.Release(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Release(%+v) error = %v, want %v", key, err, ErrInvalidKey)
		}
	}

	requestTests := []Vector{
		{},
		{CPUMillis: -1},
		{MemoryBytes: -1},
		{GuestEphemeralDiskBytes: -1},
		{VMSlots: -1},
		{BuildSlots: -1},
	}
	for _, request := range requestTests {
		if created, err := ledger.Reserve(validKey, request); created || !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Reserve(%+v) error = %v, want %v", request, err, ErrInvalidRequest)
		}
	}
}

func TestReserveAccountsForEveryDimension(t *testing.T) {
	tests := []struct {
		name   string
		vector Vector
	}{
		{name: "cpu", vector: Vector{CPUMillis: 1}},
		{name: "memory", vector: Vector{MemoryBytes: 1}},
		{name: "guest ephemeral disk", vector: Vector{GuestEphemeralDiskBytes: 1}},
		{name: "VM slots", vector: Vector{VMSlots: 1}},
		{name: "build slots", vector: Vector{BuildSlots: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capacity := Vector{
				CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: 1,
				VMSlots: 1, BuildSlots: 1,
			}
			ledger := newTestLedger(t, capacity)
			if created, err := ledger.Reserve(Key{Kind: "test", Epoch: 1, ID: "first"}, test.vector); err != nil || !created {
				t.Fatalf("first Reserve(): %v", err)
			}
			created, err := ledger.Reserve(Key{Kind: "test", Epoch: 1, ID: "second"}, test.vector)
			if created || !errors.Is(err, ErrCapacityExceeded) {
				t.Fatalf("second Reserve() error = %v, want %v", err, ErrCapacityExceeded)
			}
			if got := ledger.Snapshot().Used; got != test.vector {
				t.Fatalf("used = %+v, want %+v", got, test.vector)
			}
		})
	}
}

func TestReserveDuplicateSemantics(t *testing.T) {
	ledger := newTestLedger(t, Vector{CPUMillis: 10, MemoryBytes: 10})
	key := Key{Kind: "run", Epoch: 7, ID: "run-1"}
	request := Vector{CPUMillis: 3, MemoryBytes: 4}

	if created, err := ledger.Reserve(key, request); err != nil || !created {
		t.Fatalf("Reserve() = (%t, %v), want (true, nil)", created, err)
	}
	if created, err := ledger.Reserve(key, request); err != nil || created {
		t.Fatalf("idempotent Reserve() = (%t, %v), want (false, nil)", created, err)
	}
	if got := ledger.Snapshot().Used; got != request {
		t.Fatalf("used after replay = %+v, want %+v", got, request)
	}

	created, err := ledger.Reserve(key, Vector{CPUMillis: 4, MemoryBytes: 4})
	if created || !errors.Is(err, ErrDuplicateReservation) {
		t.Fatalf("mismatched Reserve() error = %v, want %v", err, ErrDuplicateReservation)
	}
	if got := ledger.Snapshot().Used; got != request {
		t.Fatalf("used after mismatch = %+v, want %+v", got, request)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	ledger := newTestLedger(t, Vector{CPUMillis: 10, MemoryBytes: 10})
	key := Key{Kind: "build", Epoch: 2, ID: "build-1"}
	request := Vector{CPUMillis: 3, MemoryBytes: 4}
	if _, err := ledger.Reserve(key, request); err != nil {
		t.Fatalf("Reserve(): %v", err)
	}

	if err := ledger.Release(key); err != nil {
		t.Fatalf("Release(): %v", err)
	}
	if err := ledger.Release(key); err != nil {
		t.Fatalf("replayed Release(): %v", err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Used != (Vector{}) {
		t.Fatalf("used = %+v, want zero", snapshot.Used)
	}
	if len(snapshot.Reservations) != 0 {
		t.Fatalf("reservations = %+v, want empty", snapshot.Reservations)
	}
}

func TestReserveDetectsOverflow(t *testing.T) {
	ledger := newTestLedger(t, Vector{CPUMillis: math.MaxInt64, MemoryBytes: 1})
	if _, err := ledger.Reserve(
		Key{Kind: "run", Epoch: 1, ID: "first"},
		Vector{CPUMillis: math.MaxInt64},
	); err != nil {
		t.Fatalf("first Reserve(): %v", err)
	}
	created, err := ledger.Reserve(
		Key{Kind: "run", Epoch: 1, ID: "second"},
		Vector{CPUMillis: 1},
	)
	if created || !errors.Is(err, ErrOverflow) {
		t.Fatalf("second Reserve() error = %v, want %v", err, ErrOverflow)
	}
}

func TestSnapshotIsIndependent(t *testing.T) {
	ledger := newTestLedger(t, Vector{CPUMillis: 10, MemoryBytes: 10})
	key := Key{Kind: "run", Epoch: 1, ID: "run-1"}
	request := Vector{CPUMillis: 2, MemoryBytes: 3}
	if _, err := ledger.Reserve(key, request); err != nil {
		t.Fatalf("Reserve(): %v", err)
	}

	first := ledger.Snapshot()
	delete(first.Reservations, key)
	first.Reservations[Key{Kind: "fake", Epoch: 1, ID: "fake"}] = Vector{CPUMillis: 10}
	first.Capacity.CPUMillis = 0
	first.Used.CPUMillis = 0

	second := ledger.Snapshot()
	if second.Capacity != (Vector{CPUMillis: 10, MemoryBytes: 10}) {
		t.Fatalf("capacity = %+v", second.Capacity)
	}
	if second.Used != request {
		t.Fatalf("used = %+v, want %+v", second.Used, request)
	}
	if len(second.Reservations) != 1 || second.Reservations[key] != request {
		t.Fatalf("reservations = %+v", second.Reservations)
	}
}

func TestLedgerConcurrentReserveAndRelease(t *testing.T) {
	const reservations = 200
	ledger := newTestLedger(t, Vector{
		CPUMillis: reservations, MemoryBytes: reservations,
		GuestEphemeralDiskBytes: reservations,
		VMSlots:                 reservations, BuildSlots: reservations,
	})
	request := Vector{
		CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: 1,
		VMSlots: 1, BuildSlots: 1,
	}

	runConcurrently(t, reservations, func(index int) error {
		created, err := ledger.Reserve(
			Key{Kind: "run", Epoch: 1, ID: fmt.Sprintf("run-%d", index)},
			request,
		)
		if err == nil && !created {
			return errors.New("new reservation reported as replay")
		}
		return err
	})
	snapshot := ledger.Snapshot()
	want := Vector{
		CPUMillis: reservations, MemoryBytes: reservations,
		GuestEphemeralDiskBytes: reservations,
		VMSlots:                 reservations, BuildSlots: reservations,
	}
	if snapshot.Used != want || len(snapshot.Reservations) != reservations {
		t.Fatalf("snapshot after reserve = %+v, reservations = %d", snapshot.Used, len(snapshot.Reservations))
	}

	runConcurrently(t, reservations, func(index int) error {
		key := Key{Kind: "run", Epoch: 1, ID: fmt.Sprintf("run-%d", index)}
		if err := ledger.Release(key); err != nil {
			return err
		}
		return ledger.Release(key)
	})
	snapshot = ledger.Snapshot()
	if snapshot.Used != (Vector{}) || len(snapshot.Reservations) != 0 {
		t.Fatalf("snapshot after release = %+v, reservations = %d", snapshot.Used, len(snapshot.Reservations))
	}
}

func TestLedgerConcurrentReplay(t *testing.T) {
	const goroutines = 200
	ledger := newTestLedger(t, Vector{CPUMillis: 1, MemoryBytes: 1})
	key := Key{Kind: "run", Epoch: 1, ID: "same"}
	request := Vector{CPUMillis: 1, MemoryBytes: 1}
	var createdCount atomic.Int64

	runConcurrently(t, goroutines, func(int) error {
		created, err := ledger.Reserve(key, request)
		if created {
			createdCount.Add(1)
		}
		return err
	})
	snapshot := ledger.Snapshot()
	if snapshot.Used != request || len(snapshot.Reservations) != 1 {
		t.Fatalf("snapshot = %+v, reservations = %d", snapshot.Used, len(snapshot.Reservations))
	}
	if got := createdCount.Load(); got != 1 {
		t.Fatalf("created count = %d, want 1", got)
	}
}

func newTestLedger(t *testing.T, capacity Vector) *Ledger {
	t.Helper()
	ledger, err := New(capacity)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return ledger
}

func runConcurrently(t *testing.T, count int, run func(int) error) {
	t.Helper()
	var wait sync.WaitGroup
	errorsChannel := make(chan error, count)
	for index := range count {
		wait.Go(func() {
			if err := run(index); err != nil {
				errorsChannel <- err
			}
		})
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Errorf("concurrent operation: %v", err)
	}
}
