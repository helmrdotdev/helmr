package computerhost

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type PreparationSourceClient interface {
	HostIdentity() (string, int64, error)
	PreparationSource(context.Context, workerapi.PreparationSourceRequest) (workerapi.PreparationSource, error)
}

// One private leaf owns one full lifecycle charge. State transitions and claim
// versus eviction use optional.mu. Its single job owns VM/device effects until
// done; a claimant joins that job before installing real execution authority.
type preparedLeaf struct {
	key                              workerapi.ReadyLeafKey
	identity                         workerapi.AllocationIdentity
	revision                         int64
	charge                           reservation.Vector
	expires                          time.Time
	ctx                              context.Context
	cancel                           context.CancelFunc
	done                             chan struct{}
	running, ready, claimed, reclaim bool
	err                              error
	cleanupErr                       error
	shape                            workerapi.AllocationShape
	manifest                         bootseed.Manifest
	source                           workerapi.ComputerAllocationSource // Disk keys are cleared after construction.
	program                          *computercheckpoint.Program
	machine                          vm.PreparedComputer
	device                           vm.ComputerDevice
	images                           *programSnapshots
}

func (l *preparedLeaf) reservationKey() reservation.Key {
	return reservation.Key{Kind: "prepared-computer", ID: l.identity.AllocationID, Epoch: l.identity.Epoch}
}

func (p *PreparedMachines) ApplyPreparationKeys(keys []workerapi.ReadyLeafKey) {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	// Hints are bounded and rotate at the control plane. Absence from one page
	// does not revoke a retained source; each leaf independently renews it.
	p.optional.preparationKeys = slices.Clone(keys[:min(len(keys), 32)])
}

func (p *PreparedMachines) leafTasksLocked() int {
	n := 0
	for _, leaf := range p.optional.leaves {
		if leaf.running && !leaf.ready {
			n++
		}
	}
	return n
}

// Called only by the process-root optional loop. jobs joins every launched
// build, renewal and cleanup before that root returns.
func (p *PreparedMachines) reconcilePreparedLeaves(ctx context.Context, jobs *sync.WaitGroup) {
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	policy := p.optional.policy
	for key, until := range p.optional.preparationBackoff {
		if !time.Now().Before(until) {
			delete(p.optional.preparationBackoff, key)
		}
	}
	fresh := policy.Revision > 0 && time.Since(p.optional.confirmed) < 2*workerapi.WorkerObservationInterval && !p.optional.closed
	for _, leaf := range p.optional.leaves {
		if !fresh || leaf.revision != policy.Revision || !time.Now().Before(leaf.expires) || !p.optionalUsageFitsLocked() {
			leaf.reclaim = true
			leaf.ready = false
			leaf.cancel()
		}
		if !leaf.running && leaf.reclaim && ctx.Err() == nil {
			leaf.running = true
			jobs.Go(func() { p.finishPreparedLeaf(leaf) })
		}
	}
	if !fresh || p.PreparationClient == nil || p.optional.pressure || p.optional.realWaiting > 0 || time.Now().Before(p.optional.cacheRefillAfter) || policy.Optional.IOBytesPerSecond <= 0 || int32(p.leafTasksLocked()+len(p.optional.seeds)+len(p.optional.cacheTasks)) >= policy.Optional.MaxTasks {
		return
	}
	if _, ok := p.Backend.(vm.PreparedComputerBackend); !ok {
		return
	}
	keys := p.optional.preparationKeys
	if len(keys) == 0 {
		return
	}
	for range keys {
		key := keys[p.optional.preparationNext%len(keys)]
		p.optional.preparationNext = (p.optional.preparationNext + 1) % len(keys)
		if p.optional.leaves[key] != nil || time.Now().Before(p.optional.preparationBackoff[key]) {
			continue
		}
		_, epoch, err := p.PreparationClient.HostIdentity()
		if err != nil {
			return
		}
		leafCtx, cancel := context.WithCancel(ctx)
		leaf := &preparedLeaf{key: key, revision: policy.Revision, ctx: leafCtx, cancel: cancel, done: make(chan struct{}), running: true,
			// Source fetch has a short provisional lifetime; successful admission
			// replaces it with the authenticated envelope deadline.
			expires: time.Now().Add(30 * time.Second), identity: workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: key.EnvironmentID, OwnerID: uuid.NewV7().String(), AllocationID: uuid.NewV7().String(), Epoch: epoch}}
		headroom, err := p.optionalHeadroom(policy.LocalHeadroomSlots)
		leaf.charge = p.HostSlot
		if err == nil {
			_, err = p.reserveOptionalWithCacheLocked(leaf.reservationKey(), leaf.charge, headroom)
		}
		if err != nil {
			cancel()
			return
		}
		if p.optional.leaves == nil {
			p.optional.leaves = make(map[workerapi.ReadyLeafKey]*preparedLeaf)
		}
		p.optional.leaves[key] = leaf
		jobs.Go(func() { p.runPreparedLeaf(leaf) })
		return
	}
}

func (p *PreparedMachines) readPreparationSource(ctx context.Context, leaf *preparedLeaf) (workerapi.PreparationSource, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	source, err := p.PreparationClient.PreparationSource(ctx, workerapi.PreparationSourceRequest{EnvironmentID: leaf.key.EnvironmentID, BuildID: leaf.key.BuildID})
	if err != nil {
		return source, err
	}
	host, epoch, err := p.PreparationClient.HostIdentity()
	if err != nil || source.WorkerHostID != host || source.WorkerEpoch != epoch || epoch != leaf.identity.Epoch || source.EnvironmentID != leaf.key.EnvironmentID || source.BuildID != leaf.key.BuildID || source.Manifest.EnvironmentID.String() != leaf.key.EnvironmentID || source.Manifest.BuildID.String() != leaf.key.BuildID || source.PolicyRevision != leaf.revision || !time.Now().Before(source.ExpiresAt) || source.Source.Disk.WriteKeyID != "" {
		source.Source.Disk.Clear()
		return workerapi.PreparationSource{}, errors.New("preparation source authority differs or expired")
	}
	return source, nil
}

func (p *PreparedMachines) runPreparedLeaf(leaf *preparedLeaf) {
	defer close(leaf.done)
	started := time.Now()
	envelope, err := p.readPreparationSource(leaf.ctx, leaf)
	if err == nil {
		p.optional.mu.Lock()
		leaf.expires = envelope.ExpiresAt
		p.optional.mu.Unlock()
		// Renewal owns an immutable source description, independent of build
		// state. Join it before adoption or cleanup can take physical custody.
		renewed := make(chan error, 1)
		go func() {
			err := p.renewPreparedLeaf(leaf, envelope)
			leaf.cancel()
			renewed <- err
		}()
		err = p.buildPreparedLeaf(leaf, envelope)
		if err != nil {
			leaf.cancel()
		} else {
			p.optional.mu.Lock()
			if leaf.ctx.Err() == nil && time.Now().Before(leaf.expires) && leaf.revision == p.optional.policy.Revision {
				leaf.ready = true
				p.logInfo("prepared Computer ready", "physical_owner_id", leaf.identity.AllocationID, "environment_id", leaf.key.EnvironmentID, "build_id", leaf.key.BuildID, "duration_ms", time.Since(started).Milliseconds())
			}
			p.optional.mu.Unlock()
		}
		err = errors.Join(err, <-renewed)
	}
	p.optional.mu.Lock()
	claimed := leaf.claimed
	if !claimed {
		leaf.ready = false
		leaf.reclaim = true
		leaf.err = err
	}
	p.optional.mu.Unlock()
	if !claimed {
		if err != nil && !errors.Is(err, context.Canceled) {
			p.logInfo("prepared Computer preparation failed", "physical_owner_id", leaf.identity.AllocationID, "build_id", leaf.key.BuildID, "error", err)
		}
		p.finishPreparedLeaf(leaf)
	}
}

func (p *PreparedMachines) renewPreparedLeaf(leaf *preparedLeaf, original workerapi.PreparationSource) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-leaf.ctx.Done():
			return leaf.ctx.Err()
		case <-ticker.C:
			source, err := p.readPreparationSource(leaf.ctx, leaf)
			if err == nil {
				if source.Shape != original.Shape || !reflect.DeepEqual(source.Manifest, original.Manifest) || source.Source.Disk.Root != original.Source.Disk.Root || source.Source.Disk.VersionID != original.Source.Disk.VersionID || !reflect.DeepEqual(source.Source.ImageConfig, original.Source.ImageConfig) || !reflect.DeepEqual(source.Source.BootProgram, original.Source.BootProgram) {
					err = errors.New("prepared source content changed")
				} else {
					p.optional.mu.Lock()
					if !leaf.claimed {
						leaf.expires = source.ExpiresAt
					}
					p.optional.mu.Unlock()
				}
			}
			source.Source.Disk.Clear()
			if err != nil {
				return err
			}
		}
	}
}

func (p *PreparedMachines) finishPreparedLeaf(leaf *preparedLeaf) {
	err := p.cleanupPreparedLeaf(leaf)
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	leaf.running = false
	leaf.err = err
	leaf.cleanupErr = err
	if err == nil {
		delete(p.optional.leaves, leaf.key)
		if p.optional.preparationBackoff == nil {
			p.optional.preparationBackoff = make(map[workerapi.ReadyLeafKey]time.Time)
		}
		p.optional.preparationBackoff[leaf.key] = time.Now().Add(30 * time.Second)
	}
}

// claimPreparedLeaf is atomic with eviction and reservation conversion. The
// real source, not the hint or observation, proves current generation eligibility.
func (p *PreparedMachines) claimPreparedLeaf(identity workerapi.AllocationIdentity, hostEpoch int64, delivery workerapi.ComputerAllocationDelivery, source workerapi.ComputerAllocationSource, required reservation.Vector) (*preparedLeaf, error) {
	if source.BootSeed == nil || delivery.RestoredFrom != "" || delivery.DiskOnlyRecovery {
		return nil, nil
	}
	p.optional.mu.Lock()
	defer p.optional.mu.Unlock()
	key := workerapi.ReadyLeafKey{EnvironmentID: identity.EnvironmentID, BuildID: source.BootSeed.BuildID.String()}
	leaf := p.optional.leaves[key]
	if leaf == nil || !leaf.ready || leaf.reclaim || leaf.claimed || leaf.ctx.Err() != nil || p.optional.closed || leaf.revision != p.optional.policy.Revision || time.Since(p.optional.confirmed) >= 2*workerapi.WorkerObservationInterval || !time.Now().Before(leaf.expires) || leaf.identity.Epoch != hostEpoch || leaf.shape != delivery.Shape || leaf.source.Disk.Root != source.Disk.Root || leaf.source.Disk.VersionID != source.Disk.VersionID || !reflect.DeepEqual(leaf.manifest, *source.BootSeed) || !reflect.DeepEqual(leaf.source.ImageConfig, source.ImageConfig) || !reflect.DeepEqual(leaf.source.BootProgram, source.BootProgram) {
		return nil, nil
	}
	if err := p.Reservations.AdoptOptional(leaf.reservationKey(), instanceReservationKey(identity.AllocationID, identity.Epoch), required); err != nil {
		return nil, err
	}
	leaf.claimed = true
	leaf.ready = false
	delete(p.optional.leaves, key)
	leaf.cancel()
	return leaf, nil
}

func (p *PreparedMachines) adoptPreparedLeaf(ctx context.Context, leaf *preparedLeaf, identity workerapi.AllocationIdentity, hostEpoch int64, source workerapi.ComputerAllocationSource) (vm.CheckpointableMachine, error) {
	select {
	case <-leaf.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	keys := make(map[string][]byte, len(source.Disk.Keys))
	for _, key := range source.Disk.Keys {
		if len(leaf.source.Disk.Keys) == 0 || key.Scope != leaf.source.Disk.Keys[0].Scope || keys[key.ID] != nil {
			return nil, errors.New("prepared allocation key scope differs")
		}
		keys[key.ID] = key.Key
	}
	return leaf.machine.Adopt(ctx, vm.PreparedAdoption{Binding: vm.WorkloadBinding{WorkerEpoch: hostEpoch, OwnerID: identity.AllocationID, Generation: 1, ComputerInstanceID: identity.AllocationID, VMPlatformID: leaf.shape.VMPlatformID}, ComputerID: identity.OwnerID, Root: source.Disk.Root, WriteKeyID: source.Disk.WriteKeyID, Keys: keys, Source: p.ComputerRanges})
}
