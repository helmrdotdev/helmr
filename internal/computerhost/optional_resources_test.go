package computerhost

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func optionalMachines(t *testing.T) (*PreparedMachines, workerapi.ResourcePolicy, reservation.Vector) {
	t.Helper()
	shape := reservation.Vector{CPUMillis: 1000, MemoryBytes: 100, HostDiskBytes: 100, VMSlots: 1}
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 4000, MemoryBytes: 400, HostDiskBytes: 400, VMSlots: 4})
	if err != nil {
		t.Fatal(err)
	}
	p := &PreparedMachines{Reservations: ledger, HostSlot: shape}
	policy := workerapi.ResourcePolicy{Revision: 1, LocalHeadroomSlots: 1, Optional: workerapi.OptionalResources{CPUMillis: 3000, MemoryBytes: 300, DiskBytes: 300, VMSlots: 3, MaxTasks: 3, IOBytesPerSecond: 1000}}
	p.ApplyResourcePolicy(policy)
	return p, policy, shape
}

func TestOptionalSeedPolicyReductionRetainsPhysicalResources(t *testing.T) {
	p, policy, shape := optionalMachines(t)
	key := instanceReservationKey("seed", 1)
	if err := p.reserveOptionalSeed(key, shape); err != nil {
		t.Fatal(err)
	}
	observation := p.ResourceObservation()
	if observation.OptionalDiskBytes != 100 || observation.LocalOptional != (workerapi.LocalOptionalResources{}) || observation.DiskSlotsForRealWork != 3 {
		t.Fatalf("seed reported twice or missing physical disk: %+v", observation)
	}
	policy.Revision++
	policy.Optional = workerapi.OptionalResources{}
	p.ApplyResourcePolicy(policy)
	if !p.optionalSeedReclaimRequested(key) {
		t.Fatal("old attempt did not request control-authorized reclaim")
	}
	if p.Reservations.Snapshot().Used != shape || !p.ResourceObservation().ReclaimPending {
		t.Fatal("policy change prematurely released charge")
	}
	// Simulate a cleanup failure: another observation must retain the whole charge.
	if p.ResourceObservation().OptionalDiskBytes != 100 {
		t.Fatal("cleanup failure lost charge")
	}
	if err := p.Reservations.Release(key); err != nil {
		t.Fatal(err)
	}
	p.forgetOptionalSeed(key)
	if got := p.ResourceObservation(); got.OptionalDiskBytes != 0 || got.ReclaimPending {
		t.Fatalf("joined release: %+v", got)
	}
}

func TestResourceObservationCapsSurplusDiskAtHostSlots(t *testing.T) {
	p, _, _ := optionalMachines(t)
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 4000, MemoryBytes: 400, HostDiskBytes: 10000, VMSlots: 4})
	if err != nil {
		t.Fatal(err)
	}
	p.Reservations = ledger
	if got := p.ResourceObservation().DiskSlotsForRealWork; got != 4 {
		t.Fatalf("surplus disk advertised %d slots", got)
	}
}

func TestOptionalSeedAdmissionUsesCombinedBudgetAndHeadroom(t *testing.T) {
	p, policy, shape := optionalMachines(t)
	leaf := reservation.Key{Kind: "leaf", ID: "prepared", Epoch: 1}
	if _, err := p.Reservations.ReserveOptional(leaf, shape, optionalCeiling(policy), shape); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if err := p.reserveOptionalSeed(instanceReservationKey(id, 1), shape); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.reserveOptionalSeed(instanceReservationKey("third", 1), shape); !errors.Is(err, reservation.ErrCapacityExceeded) {
		t.Fatalf("seed ignored local leaf charge: %v", err)
	}
	if !p.optionalSeedReclaimRequested(instanceReservationKey("third", 1)) {
		t.Fatal("unstarted attempt retained its grant without requesting reclaim")
	}
	if _, err := p.Reservations.Reserve(instanceReservationKey("real", 1), shape); err != nil {
		t.Fatalf("optional work borrowed real-work headroom: %v", err)
	}
}

func TestOptionalSeedCannotStartWithMissingStaleOrZeroPolicy(t *testing.T) {
	for _, mode := range []string{"missing", "stale", "zero", "tasks", "io"} {
		t.Run(mode, func(t *testing.T) {
			p, policy, shape := optionalMachines(t)
			switch mode {
			case "missing":
				p.optional.policy = workerapi.ResourcePolicy{}
			case "stale":
				p.optional.confirmed = time.Now().Add(-3 * workerapi.WorkerObservationInterval)
			case "zero":
				policy.Optional = workerapi.OptionalResources{}
				p.ApplyResourcePolicy(policy)
			case "tasks":
				policy.Optional.MaxTasks = 0
				p.ApplyResourcePolicy(policy)
			case "io":
				policy.Optional.IOBytesPerSecond = 0
				p.ApplyResourcePolicy(policy)
			}
			if err := p.reserveOptionalSeed(instanceReservationKey("seed", 1), shape); !errors.Is(err, reservation.ErrCapacityExceeded) {
				t.Fatalf("admission: %v", err)
			}
			if p.Reservations.Snapshot().Used != (reservation.Vector{}) {
				t.Fatal("rejected seed consumed physical capacity")
			}
		})
	}
}

func TestRetainedSourcePolicyAndPromotionKeepPhysicalReservation(t *testing.T) {
	p, policy, shape := optionalMachines(t)
	key := instanceReservationKey("paused", 1)
	if _, err := p.Reservations.Reserve(key, shape); err != nil {
		t.Fatal(err)
	}
	if err := p.retainCheckpointSource(key); err != nil {
		t.Fatal(err)
	}
	if p.retainedSourceReclaimRequested(key) {
		t.Fatal("fresh source reclaimed")
	}
	if got := p.ResourceObservation(); got.OptionalDiskBytes != shape.HostDiskBytes || got.LocalOptional.CPUMillis != 0 {
		t.Fatalf("source double counted: %+v", got)
	}
	if err := p.promoteCheckpointSource(key); err != nil {
		t.Fatal(err)
	}
	if p.Reservations.Snapshot().Used != shape || len(p.Reservations.Snapshot().Optional) != 0 {
		t.Fatal("promotion released capacity")
	}
	if err := p.retainCheckpointSource(key); err != nil {
		t.Fatal(err)
	}
	policy.Revision++
	policy.Optional = workerapi.OptionalResources{}
	p.ApplyResourcePolicy(policy)
	if !p.retainedSourceReclaimRequested(key) {
		t.Fatal("reduced policy did not reclaim source")
	}
	if p.Reservations.Snapshot().Used != shape {
		t.Fatal("reclaim released capacity before cleanup")
	}
}
