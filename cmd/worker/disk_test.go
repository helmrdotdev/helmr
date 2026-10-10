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
	capacity, err := partitionWorkerDiskCapacity(hostMiB, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if got := capacity.HostDiskBytes / capacity.VMGuestEphemeralDiskBytes; got != 4 {
		t.Fatalf("aggregate holds %d VMs, want four-slot exact fit without N+1", got)
	}
}

func TestAdmissionDiskFloorMatchesWorkerFilesystemContract(t *testing.T) {
	if got := admissionDiskFloorMiB(8192, 1024); got != 9216 {
		t.Fatalf("run disk floor = %d, want 9216", got)
	}
}

func TestWorkerDiskCapacityValidatesSingleVMShapeAgainstAggregate(t *testing.T) {
	capacity := workerDiskCapacity{
		VMGuestEphemeralDiskBytes: 8192 << 20,
		HostDiskBytes:             4 * (8192 << 20),
	}
	if err := capacity.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := capacity.HostDiskBytes / capacity.VMGuestEphemeralDiskBytes; got != 4 {
		t.Fatalf("aggregate holds %d VMs, want exact fit of 4", got)
	}
	capacity.HostDiskBytes = capacity.VMGuestEphemeralDiskBytes - 1
	if err := capacity.Validate(); err == nil {
		t.Fatal("single-VM disk shape larger than aggregate host capacity was accepted")
	}
}

func TestPartitionWorkerDiskCapacityDoesNotDoubleCountPhysicalDisk(t *testing.T) {
	capacity, err := partitionWorkerDiskCapacity(80*1024, 8192)
	if err != nil {
		t.Fatal(err)
	}
	accounted := capacity.HostDiskBytes
	if accounted != 80*1024*1024*1024 {
		t.Fatalf("accounted bytes %d differ from host", accounted)
	}
	if got := capacity.HostDiskBytes / capacity.VMGuestEphemeralDiskBytes; got != 10 {
		t.Fatalf("aggregate holds %d VMs, want ten-slot exact fit", got)
	}
}

func TestWorkerLifecycleFundingBoundaries(t *testing.T) {
	const perSlot = int64(216 << 30)
	for _, slots := range []int32{1, 2, 8} {
		need := perSlot * int64(slots)
		if err := validateWorkerDiskFunding(need, perSlot, slots); err != nil {
			t.Fatal(err)
		}
		if err := validateWorkerDiskFunding(need-1, perSlot, slots); err == nil {
			t.Fatal("underfunded slots accepted")
		}
		// Quarantining one owner does not reduce the configured funding requirement.
		if slots > 1 && validateWorkerDiskFunding(need-perSlot, perSlot, slots) == nil {
			t.Fatal("quarantine reduced required funding")
		}
	}
	if validateWorkerDiskFunding(1<<62, 1<<62, 4) == nil {
		t.Fatal("overflow accepted")
	}
}

func TestWorkerRecoveredDiskWithholdsResidueAndReserve(t *testing.T) {
	const required = int64(1000)
	available, err := usableWorkerDiskBytes(required+100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWorkerDiskFunding(available, required, 1); err != nil {
		t.Fatal(err)
	}
	// The filesystem reports one byte less when a leftover temporary file grows.
	available, err = usableWorkerDiskBytes(required+99, 100)
	if err != nil {
		t.Fatal(err)
	}
	if validateWorkerDiskFunding(available, required, 1) == nil {
		t.Fatal("residue was ignored")
	}
}

func TestWorkerDiskPartitionRejectsByteOverflow(t *testing.T) {
	if _, err := partitionWorkerDiskCapacity(1<<44, 1); err == nil {
		t.Fatal("host size overflow accepted")
	}
	if _, err := partitionWorkerDiskCapacity(1, 1<<44); err == nil {
		t.Fatal("scratch size overflow accepted")
	}
	if _, err := advertisedWorkerDiskMiB(t.TempDir(), 1<<44, 1); err == nil {
		t.Fatal("configured size overflow accepted")
	}
}
