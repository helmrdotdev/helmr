package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func advertisedWorkerDiskMiB(workDir string, configuredMiB int64, reserveMiB int64) (int64, error) {
	if reserveMiB <= 0 {
		return 0, errors.New("worker disk reserve must be positive")
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

func capGuestEphemeralDiskCapacity(capacity workerDiskCapacity, reserve, physicalCapacity uint64) (workerDiskCapacity, error) {
	if err := capacity.Validate(); err != nil {
		return workerDiskCapacity{}, err
	}
	if reserve == 0 || reserve >= uint64(capacity.HostGuestEphemeralDiskBytes) {
		return workerDiskCapacity{}, errors.New("worker disk reserve consumes aggregate capacity")
	}
	capacity.HostGuestEphemeralDiskBytes -= int64(reserve)
	if physicalCapacity < uint64(capacity.HostGuestEphemeralDiskBytes) {
		capacity.HostGuestEphemeralDiskBytes = int64(physicalCapacity)
	}
	if err := capacity.Validate(); err != nil {
		return workerDiskCapacity{}, err
	}
	return capacity, nil
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

// workerDiskCapacity keeps a single-VM shape separate from the aggregate host
// pools consumed by dispatch. This prevents a worker with N VM slots from
// advertising only one VM's disk as its total capacity.
type workerDiskCapacity struct {
	VMGuestEphemeralDiskBytes   int64
	HostGuestEphemeralDiskBytes int64
}

func partitionWorkerDiskCapacity(hostMiB, vmMiB, cacheBytes int64) (workerDiskCapacity, error) {
	const mib = int64(1024 * 1024)
	if hostMiB <= 0 || vmMiB <= 0 || cacheBytes < 0 || cacheBytes > hostMiB*mib {
		return workerDiskCapacity{}, errors.New("worker physical disk budget is invalid")
	}
	capacity := workerDiskCapacity{
		VMGuestEphemeralDiskBytes:   vmMiB * mib,
		HostGuestEphemeralDiskBytes: hostMiB*mib - cacheBytes,
	}
	return capacity, capacity.Validate()
}

func (c workerDiskCapacity) Validate() error {
	if c.VMGuestEphemeralDiskBytes <= 0 || c.HostGuestEphemeralDiskBytes <= 0 {
		return errors.New("worker disk capacity fields must be positive")
	}
	if c.VMGuestEphemeralDiskBytes > c.HostGuestEphemeralDiskBytes {
		return errors.New("single-VM disk shape exceeds aggregate host capacity")
	}
	return nil
}
