package main

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
)

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
	capacity := compute.WorkerDiskCapacity{
		VMGuestEphemeralDiskBytes:   8192 << 20,
		HostGuestEphemeralDiskBytes: hostMiB << 20,
	}
	if !capacity.FitsVMs(4) {
		t.Fatal("explicit reserve was not removed before four-slot exact fit")
	}
	if capacity.FitsVMs(5) {
		t.Fatal("N+1 VM fit past net aggregate host capacity")
	}
}

func TestAdmissionDiskFloorMatchesWorkerFilesystemContract(t *testing.T) {
	if got := admissionDiskFloorMiB(false, 8192, 1024); got != 9216 {
		t.Fatalf("run disk floor = %d, want 9216", got)
	}
	if got := admissionDiskFloorMiB(true, 32768, 1024); got != 32768 {
		t.Fatalf("build disk floor = %d, want 32768", got)
	}
	if got := admissionDiskFloorMiB(true, 65536, 1024); got != 65536 {
		t.Fatalf("mixed worker disk floor = %d, want 65536", got)
	}
}

func TestCapGuestEphemeralDiskCapacityUsesStablePhysicalCapacity(t *testing.T) {
	capacity := compute.WorkerDiskCapacity{
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

func TestWorkerCacheBudgetsShareDefaultDiskBudget(t *testing.T) {
	substrate, artifact := workerCacheBudgetsBytes(0, 0, 96000)
	if substrate != 32000*1024*1024 || artifact != 16000*1024*1024 {
		t.Fatalf("cache budgets substrate=%d artifact=%d, want 32000/16000 MiB", substrate, artifact)
	}
}

func TestWorkerCacheBudgetsShareSmallDiskBudget(t *testing.T) {
	substrate, artifact := workerCacheBudgetsBytes(0, 0, 400)
	if substrate+artifact != 200*1024*1024 {
		t.Fatalf("total cache budget = %d, want half of small host disk", substrate+artifact)
	}
}
