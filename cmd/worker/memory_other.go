//go:build !linux

package main

import "errors"

func physicalWorkerMemoryMiB() (int64, error) {
	return 0, errors.New("worker host memory inspection requires Linux")
}
