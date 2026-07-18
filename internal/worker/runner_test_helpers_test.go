package worker

import (
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/compute"
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
		RuntimeArch: "arm64", RuntimeABI: "helmr.firecracker.snapshot.v0",
		KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", CNIProfile: "helmr/v0",
		MaxVCPUs: 2, MaxMemoryMiB: 2048, MaxDiskMiB: 20480, VMMaxDiskMiB: 20480, ExecutionSlotsAvailable: 1,
		VMMilliCPU: 2000, VMMemoryMiB: 2048,
		ScratchBytes: 20480 << 20, VMMaxScratchBytes: 20480 << 20,
		Network: api.WorkerNetworkCapabilities{Internet: true, BlockInternet: true, DenyCIDRs: true},
	}
}

func testRequirements() compute.RunRuntimeRequirements {
	return compute.RunRuntimeRequirements{
		Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 512, DiskMiB: 1024, Slots: 1},
		Runtime:   compute.RuntimeSelector{ID: "sha256:runtime", Arch: "arm64", ABI: "helmr.firecracker.snapshot.v0", KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", CNIProfile: "helmr/v0"},
		Network:   compute.DefaultNetworkPolicy(),
	}
}
