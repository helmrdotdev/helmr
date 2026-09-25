package executor

import (
	"github.com/helmrdotdev/helmr/internal/capacity"
	"os"
	"path/filepath"
	"testing"
)

func TestLostComputerOwnerRetainsAttachmentEvidenceAndCapacity(t *testing.T) {
	const id = "01950000-0000-7000-8000-000000000001"
	ledger, err := capacity.New(capacity.Vector{CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.Reserve(computerStagingKey(id, 1), capacity.Vector{GuestEphemeralDiskBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	pool := &PreparedRuntimePool{TempDir: t.TempDir(), Capacity: ledger}
	dir := pool.computerPreparationDirectory(id, 1)
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "config.json")
	if err = os.WriteFile(evidence, []byte("retained helper claim"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = pool.releaseRuntimeCapacity(id, 1); err == nil {
		t.Fatal("missing map treated as helper exclusion")
	}
	if _, err = os.Stat(evidence); err != nil {
		t.Fatal("lost reconciliation evidence", err)
	}
	if len(ledger.Snapshot().Reservations) != 1 {
		t.Fatal("released uncertain reservation")
	}
}
