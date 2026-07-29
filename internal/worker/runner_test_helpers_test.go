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
		CPUMillis:               capabilities.MaxVCPUs * 1000,
		MemoryBytes:             capabilities.MaxMemoryMiB * 1024 * 1024,
		GuestEphemeralDiskBytes: capabilities.GuestEphemeralDiskBytes,
		VMSlots:                 int64(capabilities.ExecutionSlotsAvailable),
		BuildSlots:              int64(capabilities.MaxBuildExecutors),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resources
}

func testCapabilities() api.WorkerCapabilities {
	return api.WorkerCapabilities{
		ProtocolVersion: api.CurrentWorkerProtocolVersion, RuntimeID: "sha256:runtime",
		RuntimeArch: "x86_64", RuntimeABI: "helmr.firecracker.snapshot.v0",
		KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", CNIProfile: "helmr/v0",
		MaxVCPUs: 3, MaxMemoryMiB: 4096, ExecutionSlotsAvailable: 1,
		VMMilliCPU: 2000, VMMemoryMiB: 2048,
		GuestEphemeralDiskBytes: 32768 << 20, VMGuestEphemeralDiskBytes: 32768 << 20,
		Network: api.WorkerNetworkCapabilities{Internet: true, BlockInternet: true, DenyCIDRs: true},
	}
}
