package compute

import "testing"

func TestResourceVectorFits(t *testing.T) {
	capacity := ResourceVector{
		MilliCPU:  4000,
		MemoryMiB: 8192,
		DiskMiB:   20480,
		Slots:     4,
	}

	if !capacity.Fits(ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 2}) {
		t.Fatal("expected capacity to satisfy smaller request")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 5000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 1}) {
		t.Fatal("expected CPU overcommit to fail")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 1000, MemoryMiB: 16384, DiskMiB: 1024, Slots: 1}) {
		t.Fatal("expected memory overcommit to fail")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 1000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 5}) {
		t.Fatal("expected slot overcommit to fail")
	}
}

func TestWorkerInstanceCanSchedule(t *testing.T) {
	requirements := RunRuntimeRequirements{
		Resources: ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		Runtime:   RuntimeSelector{Arch: "x86_64", ABI: "linux", KernelDigest: "sha256:kernel", RootfsDigest: "sha256:rootfs", CNIProfile: "default"},
		Placement: Placement{
			Region:       "us-east-1",
			Tags:         map[string]string{"pool": "standard"},
			DedicatedKey: "tenant-a",
			SnapshotKey:  "snapshot-a",
		},
	}

	host := WorkerInstance{
		Status:    WorkerInstanceStatusActive,
		Region:    "us-east-1",
		Available: ResourceVector{MilliCPU: 2000, MemoryMiB: 2048, Slots: 2},
		Runtime:   RuntimeSelector{Arch: "x86_64", ABI: "linux", KernelDigest: "sha256:kernel", RootfsDigest: "sha256:rootfs", CNIProfile: "default"},
		Labels:    map[string]string{"pool": "standard", "dedicated_key": "tenant-a", "snapshot_key": "snapshot-a"},
	}
	if !host.CanSchedule(requirements) {
		t.Fatal("expected active matching host with enough resources to schedule")
	}

	host.Status = WorkerInstanceStatusDraining
	if host.CanSchedule(requirements) {
		t.Fatal("expected draining host to reject scheduling")
	}

	host.Status = WorkerInstanceStatusActive
	host.Region = "us-west-2"
	if host.CanSchedule(requirements) {
		t.Fatal("expected region mismatch to reject scheduling")
	}

	host.Region = "us-east-1"
	host.Runtime.RootfsDigest = "sha256:other-rootfs"
	if host.CanSchedule(requirements) {
		t.Fatal("expected runtime mismatch to reject scheduling")
	}

	host.Runtime.RootfsDigest = "sha256:rootfs"
	host.Labels["pool"] = "gpu"
	if host.CanSchedule(requirements) {
		t.Fatal("expected placement tag mismatch to reject scheduling")
	}
}
