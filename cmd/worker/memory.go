package main

import "fmt"

func validateWorkerMemoryMiB(configuredMiB, physicalMiB int64) error {
	if configuredMiB > physicalMiB {
		return fmt.Errorf("configured worker memory %d MiB exceeds physical host memory %d MiB", configuredMiB, physicalMiB)
	}
	return nil
}
