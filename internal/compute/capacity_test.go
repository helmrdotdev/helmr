package compute

import "testing"

func TestWorkerDiskCapacityExactFitAndNPlusOne(t *testing.T) {
	capacity := WorkerDiskCapacity{
		VMGuestEphemeralDiskBytes:   8192 << 20,
		HostGuestEphemeralDiskBytes: 4 * (8192 << 20),
	}
	if err := capacity.Validate(); err != nil {
		t.Fatal(err)
	}
	if !capacity.FitsVMs(4) {
		t.Fatal("four exact-fit VMs were rejected")
	}
	if capacity.FitsVMs(5) {
		t.Fatal("N+1 VM exceeded aggregate host capacity")
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
	if !capacity.FitsVMs(8) {
		t.Fatal("one disk dimension should retain eight-slot exact fit")
	}
}
