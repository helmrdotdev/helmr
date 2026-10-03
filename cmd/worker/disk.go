package main

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func advertisedWorkerDiskMiB(workDir string, configuredMiB int64, reserveMiB int64) (int64, error) {
	if reserveMiB <= 0 || reserveMiB > math.MaxInt64/(1<<20) || configuredMiB < 0 || configuredMiB > math.MaxInt64/(1<<20) {
		return 0, errors.New("invalid worker disk capacity or reserve")
	}
	totalMiB := configuredMiB
	if configuredMiB > 0 {
		if reserveMiB >= configuredMiB {
			return 0, errors.New("worker disk reserve consumes configured capacity")
		}
		return configuredMiB - reserveMiB, nil
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(workDir, &stat); err != nil {
		return 0, err
	}
	totalMiB = int64((stat.Blocks * uint64(stat.Bsize)) / (1024 * 1024))
	if totalMiB <= 0 {
		return 0, errors.New("worker filesystem has no disk capacity")
	}
	if reserveMiB >= totalMiB {
		return 0, errors.New("worker disk reserve consumes filesystem capacity")
	}
	advertisedMiB := totalMiB - reserveMiB
	if advertisedMiB <= 0 {
		return 0, errors.New("worker filesystem has no advertisable disk capacity")
	}
	return advertisedMiB, nil
}

func admissionDiskFloorMiB(vmScratchMiB, reserveMiB int64) int64 {
	return reserveMiB + vmScratchMiB
}

func workerCacheBudgetBytes(configuredMiB int64, hostDiskMiB int64, numerator int64, denominator int64, floorMiB int64, ceilingMiB int64) int64 {
	if configuredMiB > 0 {
		return configuredMiB * 1024 * 1024
	}
	return workerDerivedCacheBudgetBytes(hostDiskMiB, numerator, denominator, floorMiB, ceilingMiB)
}

func workerDerivedCacheBudgetBytes(hostDiskMiB int64, numerator int64, denominator int64, floorMiB int64, ceilingMiB int64) int64 {
	budgetMiB := ceilingMiB
	if hostDiskMiB > 0 && denominator > 0 {
		budgetMiB = hostDiskMiB * numerator / denominator
		if hostDiskMiB < floorMiB*2 {
			budgetMiB = hostDiskMiB / 2
		} else if budgetMiB < floorMiB {
			budgetMiB = floorMiB
		}
		if budgetMiB > ceilingMiB {
			budgetMiB = ceilingMiB
		}
	}
	if budgetMiB <= 0 {
		return 0
	}
	return budgetMiB * 1024 * 1024
}

// workerDiskCapacity distinguishes the guest scratch shape from the physical
// supply shared by every stage of an Instance's lifetime.
type workerDiskCapacity struct {
	VMGuestEphemeralDiskBytes int64
	HostDiskBytes             int64
}

func partitionWorkerDiskCapacity(hostMiB, vmMiB, cacheBytes int64) (workerDiskCapacity, error) {
	const mib = int64(1024 * 1024)
	if hostMiB <= 0 || vmMiB <= 0 || hostMiB > math.MaxInt64/mib || vmMiB > math.MaxInt64/mib || cacheBytes < 0 || cacheBytes > hostMiB*mib {
		return workerDiskCapacity{}, errors.New("worker physical disk budget is invalid")
	}
	capacity := workerDiskCapacity{
		VMGuestEphemeralDiskBytes: vmMiB * mib,
		HostDiskBytes:             hostMiB*mib - cacheBytes,
	}
	return capacity, capacity.Validate()
}

func (c workerDiskCapacity) Validate() error {
	if c.VMGuestEphemeralDiskBytes <= 0 || c.HostDiskBytes <= 0 {
		return errors.New("worker disk capacity fields must be positive")
	}
	if c.VMGuestEphemeralDiskBytes > c.HostDiskBytes {
		return errors.New("single-VM disk shape exceeds aggregate host capacity")
	}
	return nil
}

// Validate every configured slot, even when recovery quarantines some owners.
// Available bytes already exclude residue; funding the original slot count also
// leaves room for uncertain writers to finish within their lifecycle bounds.
func validateWorkerDiskFunding(supply, perSlot int64, slots int32) error {
	if supply < 0 || perSlot <= 0 || slots <= 0 || perSlot > math.MaxInt64/int64(slots) {
		return errors.New("invalid worker lifecycle disk capacity")
	}
	required := perSlot * int64(slots)
	if supply < required {
		return fmt.Errorf("worker lifecycle disk capacity: have %d bytes, require %d bytes for %d slots", supply, required, slots)
	}
	return nil
}

func availableWorkerDiskBytes(workDir string, reserveBytes, cacheBytes int64) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(workDir, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 || uint64(stat.Bavail) > uint64(math.MaxInt64)/uint64(stat.Bsize) {
		return 0, errors.New("worker filesystem available capacity overflow")
	}
	return usableWorkerDiskBytes(int64(stat.Bavail)*int64(stat.Bsize), reserveBytes, cacheBytes)
}

func usableWorkerDiskBytes(available, reserve, cache int64) (int64, error) {
	if available < 0 || reserve <= 0 || cache < 0 || reserve > available || cache > available-reserve {
		return 0, errors.New("worker filesystem cannot fund reserve and cache")
	}
	return available - reserve - cache, nil
}
