//go:build linux

package main

import "golang.org/x/sys/unix"

func physicalWorkerMemoryMiB() (int64, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, err
	}
	return int64(uint64(info.Totalram) * uint64(info.Unit) / (1024 * 1024)), nil
}
