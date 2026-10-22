package computerhost

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var errOptionalReclaimed = errors.New("optional resources reclaimed")

type optionalSeed struct {
	cleanupErr error
	revision   int64
	stop       bool
}

type optionalResources struct {
	mu                  sync.Mutex
	policy              workerapi.ResourcePolicy
	confirmed           time.Time
	seeds               map[reservation.Key]*optionalSeed
	pressure            bool
	transferNext        time.Time
	cacheRefillAfter    time.Time
	cacheTasks          map[*optionalCacheTask]struct{}
	cacheReclaimPending bool
	cacheError          error
	realWaiting         int
	closed              bool
	leaves              map[workerapi.ReadyLeafKey]*preparedLeaf
	preparationKeys     []workerapi.ReadyLeafKey
	preparationNext     int
	preparationBackoff  map[workerapi.ReadyLeafKey]time.Time
}

// ApplyResourcePolicy acknowledges an authenticated policy observation. Holding
// this lock through optional admission makes revision changes atomic with local
// reservations. Existing physical reservations survive every policy change.
func (p *PreparedMachines) ApplyResourcePolicy(policy workerapi.ResourcePolicy) {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	if policy.Revision < p.optional.policy.Revision {
		return
	}
	p.optional.policy = policy
	p.optional.confirmed = time.Now()
}

func optionalCeiling(policy workerapi.ResourcePolicy) reservation.Vector {
	return reservation.Vector{CPUMillis: policy.Optional.CPUMillis, MemoryBytes: policy.Optional.MemoryBytes, HostDiskBytes: policy.Optional.DiskBytes, VMSlots: int64(policy.Optional.VMSlots)}
}

func (p *PreparedMachines) optionalHeadroom(slots int32) (reservation.Vector, error) {
	if slots < 0 {
		return reservation.Vector{}, reservation.ErrInvalidRequest
	}
	n := int64(slots)
	v := p.HostSlot
	for _, x := range []int64{v.CPUMillis, v.MemoryBytes, v.HostDiskBytes, v.VMSlots} {
		if x < 0 || (n > 0 && x > math.MaxInt64/n) {
			return reservation.Vector{}, reservation.ErrOverflow
		}
	}
	return reservation.Vector{CPUMillis: v.CPUMillis * n, MemoryBytes: v.MemoryBytes * n, HostDiskBytes: v.HostDiskBytes * n, VMSlots: v.VMSlots * n}, nil
}

func (p *PreparedMachines) reserveOptionalSeed(key reservation.Key, request reservation.Vector) error {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	policy := p.optional.policy
	if p.optional.seeds == nil {
		p.optional.seeds = make(map[reservation.Key]*optionalSeed)
	}
	if p.optional.seeds[key] != nil {
		return reservation.ErrDuplicateReservation
	}
	seed := &optionalSeed{revision: policy.Revision}
	defer func() { p.optional.seeds[key] = seed }()
	if policy.Revision <= 0 || time.Since(p.optional.confirmed) >= 2*workerapi.WorkerObservationInterval || p.optional.pressure || p.optional.closed || p.optional.realWaiting > 0 || p.optional.cacheError != nil || policy.Optional.MaxTasks <= int32(len(p.optional.seeds)+len(p.optional.cacheTasks)+p.leafTasksLocked()) || policy.Optional.IOBytesPerSecond <= 0 {
		seed.stop = true
		p.optional.pressure = true
		return reservation.ErrCapacityExceeded
	}
	headroom, err := p.optionalHeadroom(policy.LocalHeadroomSlots)
	if err != nil {
		return err
	}
	created, err := p.reserveOptionalWithCacheLocked(key, request, headroom)
	if err != nil {
		seed.stop = true
		p.optional.pressure = true
		return err
	}
	if !created {
		return reservation.ErrDuplicateReservation
	}
	return nil
}

// forgetOptionalSeed is called only after physical cleanup and ledger release.
func (p *PreparedMachines) forgetOptionalSeed(key reservation.Key) {
	p.optional.mu.Lock()
	delete(p.optional.seeds, key)
	if len(p.optional.seeds) == 0 {
		p.optional.pressure = false
	}
	p.optional.mu.Unlock()
}

// optionalSeedReclaimRequested is sampled by the seed's existing renewal loop.
// That loop requests durable stop authority before canceling its physical work.
func (p *PreparedMachines) optionalSeedReclaimRequested(key reservation.Key) bool {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	seed := p.optional.seeds[key]
	if seed == nil {
		return false
	}
	if p.optional.closed || seed.revision != p.optional.policy.Revision || time.Since(p.optional.confirmed) >= 2*workerapi.WorkerObservationInterval {
		seed.stop = true
	}
	if seed.stop {
		p.optional.pressure = true
	}
	return seed.stop
}

func (p *PreparedMachines) requestOptionalSeedReclaim(key reservation.Key) {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	if seed := p.optional.seeds[key]; seed != nil {
		seed.stop = true
		p.optional.pressure = true
	}
}

// Transfer failures can surface later as guest I/O errors, after freshness has
// recovered. Retain the stop decision independently of those error paths.
func (p *PreparedMachines) fenceOptionalTransfersLocked(revision int64) {
	for _, leaf := range p.optional.leaves {
		if leaf.revision == revision {
			leaf.reclaim = true
			leaf.ready = false
			leaf.cancel()
		}
	}
	for _, seed := range p.optional.seeds {
		if seed.revision == revision {
			seed.stop = true
			p.optional.pressure = true
		}
	}
}

func (p *PreparedMachines) ResourceObservation() workerapi.ResourceObservation {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	result := workerapi.ResourceObservation{PolicyRevision: p.optional.policy.Revision, ReclaimPending: p.optional.pressure || p.optional.cacheReclaimPending || p.optional.cacheError != nil, LocalOptional: workerapi.LocalOptionalResources{Tasks: int32(len(p.optional.cacheTasks))}, ReadyLeafKeys: []workerapi.ReadyLeafKey{}}
	if p.Reservations == nil {
		return result
	}
	snapshot := p.Reservations.Snapshot()
	for key, v := range snapshot.Optional {
		result.OptionalDiskBytes += v.HostDiskBytes
		if key.Kind == "prepared-computer" {
			result.LocalOptional.CPUMillis += v.CPUMillis
			result.LocalOptional.MemoryBytes += v.MemoryBytes
			result.LocalOptional.VMSlots += int32(v.VMSlots)
		}
	}
	result.LocalOptional.Tasks += int32(p.leafTasksLocked())
	for _, leaf := range p.optional.leaves {
		if leaf.reclaim || leaf.err != nil {
			result.ReclaimPending = true
		}
		if leaf.ready && !leaf.reclaim && leaf.ctx.Err() == nil && time.Now().Before(leaf.expires) && len(result.ReadyLeafKeys) < 32 {
			result.ReadyLeafKeys = append(result.ReadyLeafKeys, leaf.key)
		}
	}
	if p.HostSlot.HostDiskBytes > 0 {
		result.DiskSlotsForRealWork = int32(min(int64(math.MaxInt32), snapshot.Capacity.VMSlots, max(int64(0), snapshot.Capacity.HostDiskBytes-result.OptionalDiskBytes)/p.HostSlot.HostDiskBytes))
	}
	return result
}

type optionalCacheTask struct{ cancel context.CancelFunc }

func (p *PreparedMachines) CloseOptionalAdmission() {
	p.optional.mu.Lock()
	p.optional.closed = true
	for _, leaf := range p.optional.leaves {
		leaf.cancel()
	}
	p.optional.mu.Unlock()
}

// OptionalCleanupHealth fences new placement while failed physical cleanup
// retains local resources. Ordinary reclaim in progress does not pause admission.
func (p *PreparedMachines) OptionalCleanupHealth() error {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	if p.optional.cacheError != nil {
		return p.optional.cacheError
	}
	for _, leaf := range p.optional.leaves {
		if leaf.cleanupErr != nil {
			return leaf.cleanupErr
		}
	}
	for _, seed := range p.optional.seeds {
		if seed.cleanupErr != nil {
			return seed.cleanupErr
		}
	}
	return nil
}
