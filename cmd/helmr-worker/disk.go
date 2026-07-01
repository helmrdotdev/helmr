package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func advertisedWorkerDiskMiB(workDir string, configuredMiB int64) (int64, error) {
	if configuredMiB > 0 {
		return configuredMiB, nil
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(workDir, &stat); err != nil {
		return 0, err
	}
	availableMiB := int64((stat.Bavail * uint64(stat.Bsize)) / (1024 * 1024))
	if availableMiB <= 0 {
		return 0, errors.New("worker filesystem has no available disk capacity")
	}
	reserveMiB := max(availableMiB/10, 1024)
	if reserveMiB >= availableMiB {
		reserveMiB = availableMiB / 2
	}
	advertisedMiB := availableMiB - reserveMiB
	if advertisedMiB <= 0 {
		return 0, errors.New("worker filesystem has no advertisable disk capacity")
	}
	return advertisedMiB, nil
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
