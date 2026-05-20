package main

import "testing"

func TestAdvertisedWorkerDiskMiBUsesConfiguredValue(t *testing.T) {
	got, err := advertisedWorkerDiskMiB(t.TempDir(), 1234)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1234 {
		t.Fatalf("disk MiB = %d, want 1234", got)
	}
}

func TestAdvertisedWorkerDiskMiBDetectsFilesystemCapacity(t *testing.T) {
	got, err := advertisedWorkerDiskMiB(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Fatalf("disk MiB = %d, want positive", got)
	}
}
