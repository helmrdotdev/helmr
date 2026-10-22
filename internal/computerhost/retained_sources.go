package computerhost

import (
	"time"

	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The allocation owner keeps its paused VM and exact checkpoint cut. This map
// carries only optional-reclaim intent; the existing owner still joins cleanup.
func (p *PreparedMachines) retainCheckpointSource(key reservation.Key) error {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	policy := p.optional.policy
	if p.Reservations == nil || policy.Revision <= 0 || time.Since(p.optional.confirmed) >= 2*workerapi.WorkerObservationInterval || p.optional.closed || p.optional.pressure || p.optional.realWaiting > 0 || p.optional.cacheError != nil {
		return reservation.ErrCapacityExceeded
	}
	headroom, err := p.optionalHeadroom(policy.LocalHeadroomSlots)
	if err != nil {
		return err
	}
	if err = p.Reservations.RetainOptional(key, optionalCeiling(policy), headroom); err != nil {
		return err
	}
	if p.optional.sources == nil {
		p.optional.sources = make(map[reservation.Key]*optionalSeed)
	}
	p.optional.sources[key] = &optionalSeed{revision: policy.Revision}
	return nil
}

func (p *PreparedMachines) retainedSourceReclaimRequested(key reservation.Key) bool {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	source := p.optional.sources[key]
	if source == nil {
		return true
	}
	return source.stop || p.optional.closed || source.revision != p.optional.policy.Revision || time.Since(p.optional.confirmed) >= 2*workerapi.WorkerObservationInterval || !p.optionalUsageFitsLocked()
}

func (p *PreparedMachines) promoteCheckpointSource(key reservation.Key) error {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	if err := p.Reservations.PromoteOptional(key); err != nil {
		return err
	}
	delete(p.optional.sources, key)
	return nil
}

func (p *PreparedMachines) forgetCheckpointSource(key reservation.Key) {
	p.optional.mu.Lock()
	delete(p.optional.sources, key)
	p.optional.mu.Unlock()
}
