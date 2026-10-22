package computerhost

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const cacheFilesystemHeadroom = int64(64 << 20)

func optionalCacheKey() reservation.Key {
	return reservation.Key{Kind: "object-cache", Epoch: 1, ID: "worker"}
}

// resizeOptionalCacheLocked retains the old charge while canceled fills and
// reader leases still own bytes. Callers hold optional.mu; Resize never waits.
func (p *PreparedMachines) resizeOptionalCacheLocked(want int64) error {
	if p.ObjectCache == nil || p.Reservations == nil {
		return nil
	}
	key := optionalCacheKey()
	old := p.Reservations.Snapshot().Optional[key].HostDiskBytes
	if want <= cacheFilesystemHeadroom {
		want = 0
	}
	headroom, err := p.optionalHeadroom(p.optional.policy.LocalHeadroomSlots)
	if err != nil {
		return err
	}
	ceiling := optionalCeiling(p.optional.policy)
	if want < old {
		for task := range p.optional.cacheTasks {
			task.cancel()
		}
	}
	if want > old {
		request := reservation.Vector{HostDiskBytes: want}
		if old == 0 {
			_, err = p.Reservations.ReserveOptional(key, request, ceiling, headroom)
		} else {
			err = p.Reservations.ResizeOptional(key, request, ceiling, headroom)
		}
		if err != nil {
			if errors.Is(err, reservation.ErrCapacityExceeded) {
				return p.resizeOptionalCacheLocked(0)
			}
			return err
		}
		old = want
	}
	remaining, err := p.ObjectCache.Resize(max(int64(0), want-cacheFilesystemHeadroom))
	if err != nil {
		return err
	}
	retained := want
	if remaining > 0 {
		retained = max(retained, remaining+cacheFilesystemHeadroom)
	}
	p.optional.cacheReclaimPending = retained > want
	if retained == 0 {
		return p.Reservations.Release(key)
	}
	if retained < old {
		return p.Reservations.ResizeOptional(key, reservation.Vector{HostDiskBytes: retained}, ceiling, headroom)
	}
	return nil
}

// RunOptionalResources is owned and joined by the Worker supervisor. Cache
// changes are local and disposable; failures retain charges and are observed.
func (p *PreparedMachines) RunOptionalResources(ctx context.Context) error {
	var jobs sync.WaitGroup
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		p.CloseOptionalAdmission()
		jobs.Wait()
		p.optional.mu.Lock()
		p.optional.cacheError = p.resizeOptionalCacheLocked(0)
		p.optional.mu.Unlock()
	}()
	for {
		p.reconcilePreparedLeaves(ctx, &jobs)
		p.optional.mu.Lock()
		policy := p.optional.policy
		want := int64(0)
		if policy.Revision > 0 && time.Since(p.optional.confirmed) < 2*workerapi.WorkerObservationInterval && !p.optional.pressure && !p.optional.closed && p.optional.realWaiting == 0 && !time.Now().Before(p.optional.cacheRefillAfter) {
			want = policy.Optional.DiskBytes
			if p.Reservations != nil {
				snapshot := p.Reservations.Snapshot()
				for key, charge := range snapshot.Optional {
					if key != optionalCacheKey() {
						want = max(int64(0), want-charge.HostDiskBytes)
					}
				}
				want = min(want, p.ObjectCacheCapacity)
				if policy.Optional.MaxTasks == 0 || policy.Optional.IOBytesPerSecond == 0 {
					want = min(want, snapshot.Optional[optionalCacheKey()].HostDiskBytes)
				}
				headroom, err := p.optionalHeadroom(policy.LocalHeadroomSlots)
				if err != nil {
					want = 0
				} else {
					want = min(want, max(int64(0), snapshot.Capacity.HostDiskBytes-snapshot.Used.HostDiskBytes+snapshot.Optional[optionalCacheKey()].HostDiskBytes-headroom.HostDiskBytes))
				}
			}
		}
		p.optional.cacheError = p.resizeOptionalCacheLocked(want)
		p.optional.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *PreparedMachines) beginCacheTask(ctx context.Context) (context.Context, func(), bool) {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	policy := p.optional.policy
	if p.ObjectCache == nil || p.ObjectCache.Snapshot().CapacityBytes == 0 || policy.Revision <= 0 || time.Since(p.optional.confirmed) >= 2*workerapi.WorkerObservationInterval || p.optional.pressure || p.optional.closed || p.optional.realWaiting > 0 || int32(len(p.optional.cacheTasks)+len(p.optional.seeds)+p.leafTasksLocked()) >= policy.Optional.MaxTasks || policy.Optional.IOBytesPerSecond <= 0 {
		return nil, nil, false
	}
	if !p.optionalUsageFitsLocked() {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(ctx)
	task := &optionalCacheTask{cancel: cancel}
	if p.optional.cacheTasks == nil {
		p.optional.cacheTasks = make(map[*optionalCacheTask]struct{})
	}
	p.optional.cacheTasks[task] = struct{}{}
	return ctx, func() { cancel(); p.optional.mu.Lock(); delete(p.optional.cacheTasks, task); p.optional.mu.Unlock() }, true
}

// Existing cache bytes may outlive a policy decrease. They cannot authorize new
// fills while joined cleanup catches up with the newly delivered envelope.
func (p *PreparedMachines) optionalUsageFitsLocked() bool {
	if p.Reservations == nil {
		return false
	}
	headroom, err := p.optionalHeadroom(p.optional.policy.LocalHeadroomSlots)
	if err != nil {
		return false
	}
	s := p.Reservations.Snapshot()
	var optional reservation.Vector
	for _, v := range s.Optional {
		optional.CPUMillis += v.CPUMillis
		optional.MemoryBytes += v.MemoryBytes
		optional.HostDiskBytes += v.HostDiskBytes
		optional.VMSlots += v.VMSlots
	}
	cap := optionalCeiling(p.optional.policy)
	return optional.CPUMillis <= cap.CPUMillis && optional.MemoryBytes <= cap.MemoryBytes && optional.HostDiskBytes <= cap.HostDiskBytes && optional.VMSlots <= cap.VMSlots &&
		s.Used.CPUMillis <= s.Capacity.CPUMillis-headroom.CPUMillis && s.Used.MemoryBytes <= s.Capacity.MemoryBytes-headroom.MemoryBytes && s.Used.HostDiskBytes <= s.Capacity.HostDiskBytes-headroom.HostDiskBytes && s.Used.VMSlots <= s.Capacity.VMSlots-headroom.VMSlots
}

func (p *PreparedMachines) waitRealAllocationCapacity(ctx context.Context, admit func(context.Context) error, key reservation.Key, request reservation.Vector, wake *allocationWake) error {
	p.optional.mu.Lock()
	p.optional.realWaiting++
	p.optional.mu.Unlock()
	defer func() { p.optional.mu.Lock(); p.optional.realWaiting--; p.optional.mu.Unlock() }()
	return waitAllocationCapacity(ctx, func(ctx context.Context) error {
		if err := admit(ctx); err != nil {
			return err
		}
		s := p.Reservations.Snapshot()
		if request.CPUMillis > s.Capacity.CPUMillis-s.Used.CPUMillis || request.MemoryBytes > s.Capacity.MemoryBytes-s.Used.MemoryBytes || request.HostDiskBytes > s.Capacity.HostDiskBytes-s.Used.HostDiskBytes || request.VMSlots > s.Capacity.VMSlots-s.Used.VMSlots {
			p.optional.mu.Lock()
			p.optional.cacheRefillAfter = time.Now().Add(30 * time.Second)
			p.optional.cacheError = p.resizeOptionalCacheLocked(0)
			remaining := p.Reservations.Snapshot()
			if request.CPUMillis > remaining.Capacity.CPUMillis-remaining.Used.CPUMillis || request.MemoryBytes > remaining.Capacity.MemoryBytes-remaining.Used.MemoryBytes || request.HostDiskBytes > remaining.Capacity.HostDiskBytes-remaining.Used.HostDiskBytes || request.VMSlots > remaining.Capacity.VMSlots-remaining.Used.VMSlots {
				for _, leaf := range p.optional.leaves {
					leaf.reclaim = true
					leaf.ready = false
					leaf.cancel()
				}
				for _, seed := range p.optional.seeds {
					seed.stop = true
					p.optional.pressure = true
				}
			}
			p.optional.mu.Unlock()
		}
		return nil
	}, p.Reservations, key, request, wake)
}
