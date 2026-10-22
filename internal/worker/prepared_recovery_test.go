package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/firecracker/custody"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestPreparedRecoveryQuarantinesAllocationUntilPhysicalRelease(t *testing.T) {
	work := t.TempDir()
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	dir := filepath.Join(work, "vms", "guest", owner.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "owner"), []byte("instance\n"+owner.ID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := custody.PreparedAdoption{Owner: owner, AllocationID: uuid.NewV7().String(), WorkerEpoch: 3}
	if err := (custody.StateRoot{Base: work, Components: []string{"vms", "guest"}}).RecordPreparedAdoption(a); err != nil {
		t.Fatal(err)
	}
	busy := true
	ops := vmRecoveryOps{
		ownerCandidates: func(context.Context) ([]ownerCandidate, error) { return ownedVMCandidates(work, "") },
		matchingPIDs: func(id string) ([]int, error) {
			if id != owner.ID {
				t.Fatal("cleanup used logical identity")
			}
			return nil, nil
		},
		netnsExists: func(context.Context, string) (bool, error) { return false, nil },
		reclaimNetwork: func(context.Context, vm.Owner) error {
			if busy {
				return errors.New("still owned")
			}
			return nil
		},
		reclaimCgroup: func(vm.Owner) error { return nil },
		removeState:   removeOwnedRecoveryState,
	}
	evidence, err := recoverLocalVMState(t.Context(), work, "", ops)
	if err != nil || len(evidence.Quarantined) != 1 || evidence.Quarantined[0] != a.AllocationID || len(evidence.QuarantinedOwners) != 1 || evidence.QuarantinedOwners[0] != owner {
		t.Fatal(evidence, err)
	}
	if count, err := activationQuarantines(evidence); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	busy = false
	evidence, err = recoverLocalVMState(t.Context(), work, "", ops)
	if err != nil || len(evidence.Quarantined) != 0 || len(evidence.Reclaimed) != 1 || evidence.Reclaimed[0] != a.AllocationID {
		t.Fatal(evidence, err)
	}
}
