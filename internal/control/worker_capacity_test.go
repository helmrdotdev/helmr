package control

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
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

func TestNormalizeWorkerCapabilitiesRequiresOneBuildExecutor(t *testing.T) {
	for _, executors := range []int32{0, 2} {
		capabilities := testWorkerCapabilities()
		capabilities.SupportsBuild = true
		capabilities.MaxBuildExecutors = executors
		if _, err := normalizeWorkerCapabilities(capabilities); err == nil {
			t.Fatalf("build executor count %d was accepted", executors)
		}
	}
}

func TestBuildWorkerRequiresCurrentToolchainCatalog(t *testing.T) {
	policy := testBuildPolicy()
	digest, err := policy.ToolchainCatalogDigest()
	if err != nil {
		t.Fatal(err)
	}
	capabilities := testWorkerCapabilities()
	capabilities.SupportsBuild = true
	capabilities.MaxBuildExecutors = 1
	capabilities.ToolchainCatalogDigest = digest
	normalized, err := normalizeWorkerCapabilities(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{buildPolicy: policy, regionID: "us-east-1"}
	if err := server.validateWorkerBuildPolicy(normalized); err != nil {
		t.Fatal(err)
	}
	params := workerCertificationParams(
		workerActor{
			WorkerInstanceID: uuid.New(),
			WorkerGroupID:    "workers",
			WorkerEpoch:      1,
		},
		api.WorkerActivateRequest{},
		normalized,
	)
	expected, err := deployment.SHA256DigestBytes(digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(params.ToolchainCatalogDigest, expected) {
		t.Fatalf("toolchain catalog digest = %x, want %x", params.ToolchainCatalogDigest, expected)
	}

	capabilities.ToolchainCatalogDigest = "sha256:" + strings.Repeat("7", 64)
	normalized, err = normalizeWorkerCapabilities(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.validateWorkerBuildPolicy(normalized); err == nil {
		t.Fatal("validateWorkerBuildPolicy accepted another registry")
	}
}

func TestRunOnlyWorkerRejectsToolchainCatalog(t *testing.T) {
	capabilities := testWorkerCapabilities()
	capabilities.SupportsRun = true
	capabilities.MaxRuntimeStarts = 1
	capabilities.ToolchainCatalogDigest = "sha256:" + strings.Repeat("7", 64)
	if _, err := normalizeWorkerCapabilities(capabilities); err == nil {
		t.Fatal("normalizeWorkerCapabilities accepted run-only toolchain catalog")
	}
}
