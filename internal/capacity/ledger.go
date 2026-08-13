package capacity

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
)

var (
	ErrInvalidCapacity      = errors.New("invalid capacity")
	ErrInvalidKey           = errors.New("invalid reservation key")
	ErrInvalidRequest       = errors.New("invalid reservation request")
	ErrDuplicateReservation = errors.New("reservation key already has a different vector")
	ErrCapacityExceeded     = errors.New("capacity exceeded")
	ErrOverflow             = errors.New("capacity arithmetic overflow")
)

type Vector struct {
	CPUMillis               int64
	MemoryBytes             int64
	GuestEphemeralDiskBytes int64
	VMSlots                 int64
}

type Key struct {
	Kind  string
	Epoch int64
	ID    string
}

type Snapshot struct {
	Capacity     Vector
	Used         Vector
	Reservations map[Key]Vector
}

type Ledger struct {
	mu           sync.Mutex
	capacity     Vector
	used         Vector
	reservations map[Key]Vector
}

func New(capacity Vector) (*Ledger, error) {
	if capacity.CPUMillis <= 0 || capacity.MemoryBytes <= 0 ||
		capacity.GuestEphemeralDiskBytes < 0 ||
		capacity.VMSlots < 0 {
		return nil, ErrInvalidCapacity
	}

	return &Ledger{
		capacity:     capacity,
		reservations: make(map[Key]Vector),
	}, nil
}

func (l *Ledger) Reserve(key Key, request Vector) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	if err := validateRequest(request); err != nil {
		return false, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if reserved, ok := l.reservations[key]; ok {
		if reserved == request {
			return false, nil
		}
		return false, ErrDuplicateReservation
	}

	next, err := add(l.used, request)
	if err != nil {
		return false, err
	}
	if !fits(next, l.capacity) {
		return false, ErrCapacityExceeded
	}

	l.used = next
	l.reservations[key] = request
	return true, nil
}

func (l *Ledger) Release(key Key) error {
	if err := validateKey(key); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	reserved, ok := l.reservations[key]
	if !ok {
		return nil
	}

	l.used = subtract(l.used, reserved)
	delete(l.reservations, key)
	return nil
}

func (l *Ledger) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	reservations := make(map[Key]Vector, len(l.reservations))
	maps.Copy(reservations, l.reservations)
	return Snapshot{
		Capacity:     l.capacity,
		Used:         l.used,
		Reservations: reservations,
	}
}

func validateKey(key Key) error {
	if key.Kind == "" || strings.TrimSpace(key.Kind) != key.Kind ||
		key.Epoch <= 0 ||
		key.ID == "" || strings.TrimSpace(key.ID) != key.ID {
		return ErrInvalidKey
	}
	return nil
}

func validateRequest(request Vector) error {
	values := [...]int64{
		request.CPUMillis,
		request.MemoryBytes,
		request.GuestEphemeralDiskBytes,
		request.VMSlots,
	}
	positive := false
	for _, value := range values {
		if value < 0 {
			return ErrInvalidRequest
		}
		positive = positive || value > 0
	}
	if !positive {
		return ErrInvalidRequest
	}
	return nil
}

func add(left, right Vector) (Vector, error) {
	fields := [...]struct {
		name  string
		left  int64
		right int64
	}{
		{"cpu", left.CPUMillis, right.CPUMillis},
		{"memory", left.MemoryBytes, right.MemoryBytes},
		{"guest ephemeral disk", left.GuestEphemeralDiskBytes, right.GuestEphemeralDiskBytes},
		{"VM slots", left.VMSlots, right.VMSlots},
	}
	sums := [len(fields)]int64{}
	for index, field := range fields {
		if field.right > math.MaxInt64-field.left {
			return Vector{}, fmt.Errorf("%w: %s", ErrOverflow, field.name)
		}
		sums[index] = field.left + field.right
	}
	return Vector{
		CPUMillis:               sums[0],
		MemoryBytes:             sums[1],
		GuestEphemeralDiskBytes: sums[2],
		VMSlots:                 sums[3],
	}, nil
}

func subtract(left, right Vector) Vector {
	return Vector{
		CPUMillis:               left.CPUMillis - right.CPUMillis,
		MemoryBytes:             left.MemoryBytes - right.MemoryBytes,
		GuestEphemeralDiskBytes: left.GuestEphemeralDiskBytes - right.GuestEphemeralDiskBytes,
		VMSlots:                 left.VMSlots - right.VMSlots,
	}
}

func fits(used, capacity Vector) bool {
	return used.CPUMillis <= capacity.CPUMillis &&
		used.MemoryBytes <= capacity.MemoryBytes &&
		used.GuestEphemeralDiskBytes <= capacity.GuestEphemeralDiskBytes &&
		used.VMSlots <= capacity.VMSlots
}
