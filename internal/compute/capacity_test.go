package compute

import "testing"

func TestWorkerDiskCapacityValidatesSingleVMShapeAgainstAggregate(t *testing.T) {
	capacity := WorkerDiskCapacity{
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
	capacity, err := PartitionWorkerDiskCapacity(80*1024, 8192, 16*1024*1024*1024)
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
