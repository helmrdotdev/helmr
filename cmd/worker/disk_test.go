package main

import "testing"

func TestAdvertisedWorkerDiskMiBUsesConfiguredValue(t *testing.T) {
	got, err := advertisedWorkerDiskMiB(t.TempDir(), 1234, 234)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1000 {
		t.Fatalf("disk MiB = %d, want 1000", got)
	}
}

func TestAdvertisedWorkerDiskMiBUsesFilesystemCapacity(t *testing.T) {
	got, err := advertisedWorkerDiskMiB(t.TempDir(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Fatalf("disk MiB = %d, want positive", got)
	}
}

func TestAdvertisedWorkerDiskCapacityFitsNButNotNPlusOne(t *testing.T) {
	hostMiB, err := advertisedWorkerDiskMiB(t.TempDir(), 4*8192+1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := partitionWorkerDiskCapacity(hostMiB, 8192, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := capacity.HostGuestEphemeralDiskBytes / capacity.VMGuestEphemeralDiskBytes; got != 4 {
		t.Fatalf("aggregate holds %d VMs, want four-slot exact fit without N+1", got)
	}
}

func TestAdmissionDiskFloorMatchesWorkerFilesystemContract(t *testing.T) {
	if got := admissionDiskFloorMiB(8192, 1024); got != 9216 {
		t.Fatalf("run disk floor = %d, want 9216", got)
	}
}

func TestCapGuestEphemeralDiskCapacityUsesStablePhysicalCapacity(t *testing.T) {
	capacity := workerDiskCapacity{
		VMGuestEphemeralDiskBytes:   32768 << 20,
		HostGuestEphemeralDiskBytes: 65536 << 20,
	}
	got, err := capGuestEphemeralDiskCapacity(capacity, 2048<<20, 32768<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostGuestEphemeralDiskBytes != 32768<<20 {
		t.Fatalf("guest disk capacity = %d, want %d", got.HostGuestEphemeralDiskBytes, int64(32768<<20))
	}
	capacity.HostGuestEphemeralDiskBytes = 34816 << 20
	got, err = capGuestEphemeralDiskCapacity(capacity, 2048<<20, 33000<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostGuestEphemeralDiskBytes != 32768<<20 {
		t.Fatalf("reserved guest disk capacity = %d, want %d", got.HostGuestEphemeralDiskBytes, int64(32768<<20))
	}
	if _, err := capGuestEphemeralDiskCapacity(capacity, 2048<<20, (32768<<20)-1); err == nil {
		t.Fatal("physical capacity below one VM was accepted")
	}
}

func TestWorkerCacheBudgetUsesConfiguredValue(t *testing.T) {
	got := workerCacheBudgetBytes(123, 10000, 1, 3, 4096, 32768)
	if got != 123*1024*1024 {
		t.Fatalf("budget bytes = %d, want configured MiB", got)
	}
}

func TestWorkerCacheBudgetDerivesFromHostDisk(t *testing.T) {
	got := workerCacheBudgetBytes(0, 96000, 1, 3, 4096, 32768)
	if got != 32000*1024*1024 {
		t.Fatalf("budget bytes = %d, want one third of host disk", got)
	}
}

func TestWorkerCacheBudgetRespectsCeiling(t *testing.T) {
	got := workerCacheBudgetBytes(0, 300000, 1, 3, 4096, 32768)
	if got != 32768*1024*1024 {
		t.Fatalf("budget bytes = %d, want ceiling", got)
	}
}

func TestWorkerCacheBudgetShrinksForSmallDisk(t *testing.T) {
	got := workerCacheBudgetBytes(0, 400, 1, 3, 4096, 32768)
	if got != 200*1024*1024 {
		t.Fatalf("budget bytes = %d, want half of small host disk", got)
	}
}

func TestWorkerDiskCapacityValidatesSingleVMShapeAgainstAggregate(t *testing.T) {
	capacity := workerDiskCapacity{
		VMGuestEphemeralDiskBytes:   8192 << 20,
		HostGuestEphemeralDiskBytes: 4 * (8192 << 20),
	}
	if err := capacity.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := capacity.HostGuestEphemeralDiskBytes / capacity.VMGuestEphemeralDiskBytes; got != 4 {
		t.Fatalf("aggregate holds %d VMs, want exact fit of 4", got)
	}
	capacity.HostGuestEphemeralDiskBytes = capacity.VMGuestEphemeralDiskBytes - 1
	if err := capacity.Validate(); err == nil {
		t.Fatal("single-VM disk shape larger than aggregate host capacity was accepted")
	}
}

func TestPartitionWorkerDiskCapacityDoesNotDoubleCountPhysicalDisk(t *testing.T) {
	capacity, err := partitionWorkerDiskCapacity(80*1024, 8192, 16*1024*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	accounted := capacity.HostGuestEphemeralDiskBytes + 16*1024*1024*1024
	if accounted > 80*1024*1024*1024 {
		t.Fatalf("accounted bytes %d exceed host", accounted)
	}
	if got := capacity.HostGuestEphemeralDiskBytes / capacity.VMGuestEphemeralDiskBytes; got != 8 {
		t.Fatalf("aggregate holds %d VMs, want eight-slot exact fit", got)
	}
}
