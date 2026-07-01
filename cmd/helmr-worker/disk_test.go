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
