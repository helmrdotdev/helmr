package worker

import (
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/capacity"
)

func testCapacity(t interface {
	Helper()
	Fatal(...any)
}, capabilities api.WorkerCapabilities) *capacity.Ledger {
	t.Helper()
	resources, err := capacity.New(capacity.Vector{
		CPUMillis:         capabilities.MaxVCPUs * 1000,
		MemoryBytes:       capabilities.MaxMemoryMiB * 1024 * 1024,
		WorkloadDiskBytes: capabilities.MaxDiskMiB * 1024 * 1024,
		ScratchBytes:      capabilities.ScratchBytes,
		VMSlots:           int64(capabilities.ExecutionSlotsAvailable),
		BuildSlots:        int64(capabilities.MaxBuildExecutors),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resources
}

func testCapabilities() api.WorkerCapabilities {
	return api.WorkerCapabilities{
		ProtocolVersion: api.CurrentWorkerProtocolVersion, RuntimeID: "sha256:runtime",
		RuntimeArch: "aarch64", RuntimeABI: "helmr.firecracker.snapshot.v0",
		KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", CNIProfile: "helmr/v0",
		MaxVCPUs: 3, MaxMemoryMiB: 4096, MaxDiskMiB: 20480, VMMaxDiskMiB: 20480, ExecutionSlotsAvailable: 1,
		VMMilliCPU: 2000, VMMemoryMiB: 2048,
		ScratchBytes: 32768 << 20, VMMaxScratchBytes: 20480 << 20,
		Network: api.WorkerNetworkCapabilities{Internet: true, BlockInternet: true, DenyCIDRs: true},
	}
}
