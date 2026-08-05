package worker

import (
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func testCapacity(t interface {
	Helper()
	Fatal(...any)
}, capabilities workerapi.Capabilities) *capacity.Ledger {
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

func testCapabilities() workerapi.Capabilities {
	return workerapi.Capabilities{
		RuntimeID:   "sha256:runtime",
		RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0",
		KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", MaxVCPUs: 3, MaxMemoryMiB: 4096, ExecutionSlotsAvailable: 1,
		VMMilliCPU: 2000, VMMemoryMiB: 2048,
		GuestEphemeralDiskBytes: 32768 << 20, VMGuestEphemeralDiskBytes: 32768 << 20,
	}
}
