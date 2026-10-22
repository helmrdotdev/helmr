package computerhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type leafSourceTestClient struct {
	host  string
	fetch func(context.Context, workerapi.PreparationSourceRequest) (workerapi.PreparationSource, error)
}

func (c leafSourceTestClient) HostIdentity() (string, int64, error) { return c.host, 1, nil }
func (c leafSourceTestClient) PreparationSource(ctx context.Context, r workerapi.PreparationSourceRequest) (workerapi.PreparationSource, error) {
	return c.fetch(ctx, r)
}

type leafTestBackend struct{ allocationOwnerTestBackend }

func (b *leafTestBackend) PrepareComputer(context.Context, vm.MaterializeRequest) (vm.PreparedComputer, error) {
	return nil, errors.New("unexpected VM start")
}

func TestPreparedRootJoinsCanceledSourceBeforeReleasingCharge(t *testing.T) {
	p, policy, _ := optionalMachines(t)
	p.TempDir = t.TempDir()
	// Host memory includes non-page-aligned bookkeeping for this guest shape.
	// Prepared admission must use the canonical host vector, not treat it as RAM.
	memory, err := HostMemoryPerSlot(513)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCheckpointEncryptor(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	disk, err := HostDiskPerSlot(513, 32768, 65536<<20, cipher)
	if err != nil {
		t.Fatal(err)
	}
	p.HostSlot = reservation.Vector{CPUMillis: 1000, MemoryBytes: memory, HostDiskBytes: disk, VMSlots: 1}
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 4000, MemoryBytes: 4 * memory, HostDiskBytes: 4 * disk, VMSlots: 4})
	if err != nil {
		t.Fatal(err)
	}
	p.Reservations = ledger
	policy.Optional.MemoryBytes = 3 * memory
	policy.Optional.DiskBytes = 3 * disk
	policy.Optional.MaxTasks = 1
	p.ApplyResourcePolicy(policy)
	backend := &leafTestBackend{allocationOwnerTestBackend: allocationOwnerTestBackend{t: t}}
	p.Backend = backend
	started, returned := make(chan struct{}), make(chan struct{})
	p.PreparationClient = leafSourceTestClient{host: uuid.NewV7().String(), fetch: func(ctx context.Context, _ workerapi.PreparationSourceRequest) (workerapi.PreparationSource, error) {
		close(started)
		<-ctx.Done()
		if p.Reservations.Snapshot().Used.VMSlots != 1 {
			t.Error("source fetch outlived its reservation")
		}
		close(returned)
		return workerapi.PreparationSource{}, ctx.Err()
	}}
	p.ApplyPreparationKeys([]workerapi.ReadyLeafKey{{EnvironmentID: uuid.NewV7().String(), BuildID: uuid.NewV7().String()}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.RunOptionalResources(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("prepared source was not admitted for the host slot")
	}
	if observation := p.ResourceObservation(); observation.LocalOptional.Tasks != 1 || observation.LocalOptional.VMSlots != 1 || len(observation.ReadyLeafKeys) != 0 {
		t.Fatal(observation)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("optional root failed to join source reader")
	}
	if got := p.Reservations.Snapshot(); got.Used != (reservation.Vector{}) || len(got.Optional) != 0 {
		t.Fatal(got)
	}
	if backend.starts.Load() != 0 {
		t.Fatal("preparation performed ordinary boot")
	}
}

func readyLeafFixture(t *testing.T) (*PreparedMachines, *preparedLeaf, workerapi.ComputerAllocationDelivery, workerapi.ComputerAllocationSource, reservation.Vector) {
	t.Helper()
	p, policy, charge := optionalMachines(t)
	p.TempDir = t.TempDir()
	env, build := uuid.NewV7(), uuid.NewV7()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	close(done)
	manifest := bootseed.Manifest{EnvironmentID: env, BuildID: build}
	identity := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: env.String(), OwnerID: uuid.NewV7().String(), AllocationID: uuid.NewV7().String(), Epoch: 1}
	source := workerapi.ComputerAllocationSource{BootSeed: &manifest}
	leaf := &preparedLeaf{key: workerapi.ReadyLeafKey{EnvironmentID: env.String(), BuildID: build.String()}, identity: identity, revision: policy.Revision, expires: time.Now().Add(time.Minute), ctx: ctx, cancel: cancel, done: done, ready: true, running: true, manifest: manifest, source: source, charge: charge}
	p.optional.leaves = map[workerapi.ReadyLeafKey]*preparedLeaf{leaf.key: leaf}
	if _, err := p.Reservations.ReserveOptional(leaf.reservationKey(), charge, optionalCeiling(policy), charge); err != nil {
		t.Fatal(err)
	}
	identity.AllocationID = uuid.NewV7().String()
	identity.Epoch = 4
	return p, leaf, workerapi.ComputerAllocationDelivery{Identity: identity}, source, charge
}

func TestPreparedClaimRacesEvictionWithoutReleasingCharge(t *testing.T) {
	for range 50 {
		p, leaf, delivery, source, charge := readyLeafFixture(t)
		var claimed *preparedLeaf
		var claimErr error
		var jobs sync.WaitGroup
		jobs.Go(func() { claimed, claimErr = p.claimPreparedLeaf(delivery.Identity, 1, delivery, source, charge) })
		jobs.Go(p.CloseOptionalAdmission)
		jobs.Wait()
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		s := p.Reservations.Snapshot()
		if s.Used != charge {
			t.Fatal("race freed the live leaf", s)
		}
		if claimed != nil {
			if claimed != leaf || len(s.Optional) != 0 || len(p.optional.leaves) != 0 || s.Reservations[instanceReservationKey(delivery.Identity.AllocationID, delivery.Identity.Epoch)] != charge {
				t.Fatal("claim lost charge ownership", s)
			}
		} else if len(s.Optional) != 1 || len(p.optional.leaves) != 1 {
			t.Fatal("eviction released before cleanup", s)
		}
	}
}

func TestPreparedClaimRejectsExpiredChangedAndPersonalSources(t *testing.T) {
	for _, scenario := range []string{"expiry", "epoch", "policy", "root", "generation", "shape", "personal", "disk-recovery"} {
		t.Run(scenario, func(t *testing.T) {
			p, leaf, delivery, source, charge := readyLeafFixture(t)
			epoch := int64(1)
			switch scenario {
			case "expiry":
				leaf.expires = time.Now().Add(-time.Second)
			case "epoch":
				epoch++
			case "policy":
				p.ApplyResourcePolicy(workerapi.ResourcePolicy{Revision: 2})
			case "root":
				source.Disk.Root.LogicalBytes++
			case "generation":
				m := *source.BootSeed
				m.BuildID = uuid.NewV7()
				source.BootSeed = &m
			case "shape":
				delivery.Shape.CPUMillis++
			case "personal":
				delivery.RestoredFrom = uuid.NewV7().String()
			case "disk-recovery":
				delivery.DiskOnlyRecovery = true
			}
			if got, err := p.claimPreparedLeaf(delivery.Identity, epoch, delivery, source, charge); got != nil || err != nil {
				t.Fatal(got, err)
			}
			if len(p.Reservations.Snapshot().Optional) != 1 {
				t.Fatal("rejected claim altered ownership")
			}
		})
	}
}

func TestPreparedCleanupFailureRetainsChargeAndPolicyEvicts(t *testing.T) {
	p, leaf, _, _, charge := readyLeafFixture(t)
	backend := &allocationOwnerTestBackend{t: t}
	backend.failCleanup.Store(true)
	p.Backend = backend
	p.ApplyResourcePolicy(workerapi.ResourcePolicy{Revision: 2})
	var jobs sync.WaitGroup
	p.reconcilePreparedLeaves(t.Context(), &jobs)
	jobs.Wait()
	if leaf.ready || !leaf.reclaim || leaf.ctx.Err() == nil {
		t.Fatal("policy did not cancel leaf")
	}
	p.finishPreparedLeaf(leaf)
	if p.Reservations.Snapshot().Used != charge || !p.ResourceObservation().ReclaimPending {
		t.Fatal("failed cleanup advertised capacity")
	}
	backend.failCleanup.Store(false)
	p.finishPreparedLeaf(leaf)
	if p.Reservations.Snapshot().Used != (reservation.Vector{}) || len(p.optional.leaves) != 0 {
		t.Fatal("successful cleanup retained leaf")
	}
}

func TestAdoptedAllocationCleanupRemovesBothStagingOwners(t *testing.T) {
	o, client, _ := allocationOwnerFixture(t)
	done := make(chan struct{})
	close(done)
	physical := workerapi.AllocationIdentity{AllocationID: uuid.NewV7().String(), Epoch: 7}
	o.prepared = &preparedLeaf{identity: physical, done: done}
	paths := []string{o.machines.computerPreparationDirectory(physical.AllocationID, physical.Epoch), o.machines.computerPreparationDirectory(o.identity.AllocationID, o.identity.Epoch)}
	for _, dir := range paths {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "checkpoint-ciphertext"), []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.machines.Reservations.Reserve(instanceReservationKey(o.identity.AllocationID, o.identity.Epoch), reservation.Vector{HostDiskBytes: 100}); err != nil {
		t.Fatal(err)
	}
	client.beforeStop = func() {
		for _, dir := range paths {
			if _, err := os.Stat(dir); err != nil {
				t.Error("staging removed before stopped acknowledgement", err)
			}
		}
	}
	if err := o.cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range paths {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("retained staging after reservation release", err)
		}
	}
	if len(o.machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("cleanup retained real charge")
	}
}
