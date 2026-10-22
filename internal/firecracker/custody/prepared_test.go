package custody

import (
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestPreparedAdoptionIsExclusiveAndRecoveryReadable(t *testing.T) {
	work := t.TempDir()
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	makeRecoveryOwner(t, work, t.TempDir(), owner.ID)
	state := StateRoot{Base: work, Components: []string{"vms", "guest"}}
	if a, err := state.ReadPreparedAdoption(owner); err != nil || a.AllocationID != "" {
		t.Fatal(a, err)
	}
	a := PreparedAdoption{Owner: owner, AllocationID: uuid.NewV7().String(), WorkerEpoch: 7}
	if err := state.RecordPreparedAdoption(a); err != nil {
		t.Fatal(err)
	}
	if got, err := state.ReadPreparedAdoption(owner); err != nil || got != a {
		t.Fatal(got, err)
	}
	a.AllocationID = uuid.NewV7().String()
	if err := state.RecordPreparedAdoption(a); err == nil {
		t.Fatal("replaced prior allocation")
	}
	path := filepath.Join(work, "vms", "guest", owner.ID, "prepared-adoption")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReadPreparedAdoption(owner); err == nil {
		t.Fatal("partial record treated as absence")
	}
}
