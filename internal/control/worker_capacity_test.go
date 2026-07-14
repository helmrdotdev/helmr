package control

import (
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
)

func TestWorkerCertificationUsesAggregateDiskCapacity(t *testing.T) {
	workerID := uuid.New()
	params := workerCertificationParams(workerActor{WorkerInstanceID: workerID, WorkerGroupID: "workers", WorkerEpoch: 7}, api.WorkerActivateRequest{
		CertificationProfile: "helmr-runtime-v0", CertificationFingerprint: "fingerprint",
	}, api.WorkerCapabilities{
		MaxDiskMiB: 4 * 8192, VMMaxDiskMiB: 8192,
		ScratchBytes: 4 * (8192 << 20), VMMaxScratchBytes: 8192 << 20,
		ExecutionSlotsAvailable: 4,
	})
	if params.CertifiedWorkloadDiskBytes != 4*(8192<<20) {
		t.Fatalf("certified workload disk = %d, want aggregate host capacity", params.CertifiedWorkloadDiskBytes)
	}
	if params.CertifiedScratchBytes != 4*(8192<<20) {
		t.Fatalf("certified scratch = %d, want aggregate host capacity", params.CertifiedScratchBytes)
	}
	if params.MaxVmSlots != 4 {
		t.Fatalf("VM slots = %d, want 4", params.MaxVmSlots)
	}
}

func TestNormalizeWorkerCapabilitiesRejectsPerVMShapeBeyondHost(t *testing.T) {
	capabilities := testWorkerCapabilities()
	capabilities.SupportsRun = true
	capabilities.MaxRuntimeStarts = 1
	capabilities.VMMaxDiskMiB = capabilities.MaxDiskMiB + 1
	if _, err := normalizeWorkerCapabilities(capabilities); err == nil {
		t.Fatal("per-VM workload disk beyond aggregate host capacity was accepted")
	}
	capabilities.VMMaxDiskMiB = capabilities.MaxDiskMiB
	capabilities.VMMaxScratchBytes = capabilities.ScratchBytes + 1
	if _, err := normalizeWorkerCapabilities(capabilities); err == nil {
		t.Fatal("per-VM scratch beyond aggregate host capacity was accepted")
	}
}
