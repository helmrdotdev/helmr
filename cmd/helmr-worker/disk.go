package main

import (
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/compute"
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

func admissionDiskFloorMiB(supportsBuild bool, vmScratchMiB, reserveMiB int64) int64 {
	if supportsBuild {
		return max(vmScratchMiB, compute.BuildEnvelopeResources().DiskMiB)
	}
	return reserveMiB + vmScratchMiB
}

func capGuestEphemeralDiskCapacity(capacity compute.WorkerDiskCapacity, reserve, physicalCapacity uint64) (compute.WorkerDiskCapacity, error) {
	if err := capacity.Validate(); err != nil {
		return compute.WorkerDiskCapacity{}, err
	}
	if reserve == 0 || reserve >= uint64(capacity.HostGuestEphemeralDiskBytes) {
		return compute.WorkerDiskCapacity{}, errors.New("worker disk reserve consumes aggregate capacity")
	}
	capacity.HostGuestEphemeralDiskBytes -= int64(reserve)
	if physicalCapacity < uint64(capacity.HostGuestEphemeralDiskBytes) {
		capacity.HostGuestEphemeralDiskBytes = int64(physicalCapacity)
	}
	if err := capacity.Validate(); err != nil {
		return compute.WorkerDiskCapacity{}, err
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

func workerCacheBudgetsBytes(substrateConfiguredMiB int64, artifactConfiguredMiB int64, hostDiskMiB int64) (int64, int64) {
	if substrateConfiguredMiB > 0 || artifactConfiguredMiB > 0 {
		return workerCacheBudgetBytes(substrateConfiguredMiB, hostDiskMiB, 1, 3, 4096, 32768),
			workerCacheBudgetBytes(artifactConfiguredMiB, hostDiskMiB, 1, 6, 2048, 16384)
	}
	totalBytes := workerDerivedCacheBudgetBytes(hostDiskMiB, 1, 2, 6144, 49152)
	substrateBytes := totalBytes * 2 / 3
	return substrateBytes, totalBytes - substrateBytes
}
