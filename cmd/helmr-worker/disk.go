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
