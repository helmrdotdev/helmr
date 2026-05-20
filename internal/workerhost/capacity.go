package workerhost

import (
	"errors"
	"sync"

	"github.com/helmrdotdev/helmr/internal/compute"
)

var (
	ErrInvalidReservation = errors.New("invalid capacity reservation")
	ErrReservationExists  = errors.New("capacity reservation already exists")
	ErrReservationMissing = errors.New("capacity reservation missing")
)

type Reservation struct {
	ID        string
	Resources compute.ResourceVector
}

type CapacityAllocator struct {
	mu           sync.Mutex
	total        compute.ResourceVector
	available    compute.ResourceVector
	reservations map[string]compute.ResourceVector
}

func NewCapacityAllocator(total compute.ResourceVector) (*CapacityAllocator, error) {
	if err := total.Validate(true); err != nil {
		return nil, err
	}
	return &CapacityAllocator{
		total:        total,
		available:    total,
		reservations: make(map[string]compute.ResourceVector),
	}, nil
}

func (a *CapacityAllocator) Reserve(reservation Reservation) (compute.ResourceVector, error) {
	if reservation.ID == "" {
		return compute.ResourceVector{}, ErrInvalidReservation
	}
	if err := reservation.Resources.Validate(true); err != nil {
		return compute.ResourceVector{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.reservations[reservation.ID]; ok {
		return a.available, ErrReservationExists
	}
	if !a.available.Fits(reservation.Resources) {
		return a.available, compute.ErrNoCapacity
	}
	a.available = subtract(a.available, reservation.Resources)
	a.reservations[reservation.ID] = reservation.Resources
	return a.available, nil
}

func (a *CapacityAllocator) Release(id string) (compute.ResourceVector, error) {
	if id == "" {
		return compute.ResourceVector{}, ErrInvalidReservation
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	resources, ok := a.reservations[id]
	if !ok {
		return a.available, ErrReservationMissing
	}
	delete(a.reservations, id)
	a.available = add(a.available, resources)
	return a.available, nil
}

func (a *CapacityAllocator) Snapshot() (compute.ResourceVector, compute.ResourceVector, []Reservation) {
	a.mu.Lock()
	defer a.mu.Unlock()

	reservations := make([]Reservation, 0, len(a.reservations))
	for id, resources := range a.reservations {
		reservations = append(reservations, Reservation{ID: id, Resources: resources})
	}
	return a.total, a.available, reservations
}

func add(left, right compute.ResourceVector) compute.ResourceVector {
	return compute.ResourceVector{
		MilliCPU:  left.MilliCPU + right.MilliCPU,
		MemoryMiB: left.MemoryMiB + right.MemoryMiB,
		DiskMiB:   left.DiskMiB + right.DiskMiB,
		Slots:     left.Slots + right.Slots,
	}
}

func subtract(left, right compute.ResourceVector) compute.ResourceVector {
	return compute.ResourceVector{
		MilliCPU:  left.MilliCPU - right.MilliCPU,
		MemoryMiB: left.MemoryMiB - right.MemoryMiB,
		DiskMiB:   left.DiskMiB - right.DiskMiB,
		Slots:     left.Slots - right.Slots,
	}
}
