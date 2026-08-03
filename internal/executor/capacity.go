package executor

import (
	"errors"
	"math"
	"strings"

	"github.com/helmrdotdev/helmr/internal/capacity"
)

const mebibyte = int64(1024 * 1024)

func runtimeCapacityKey(id string, epoch int64) capacity.Key {
	return capacity.Key{Kind: "runtime", Epoch: epoch, ID: strings.TrimSpace(id)}
}

func runtimeCapacityVector(cpuMillis, memoryMiB, guestEphemeralDiskMiB int64) (capacity.Vector, error) {
	if memoryMiB < 0 || guestEphemeralDiskMiB < 0 ||
		memoryMiB > math.MaxInt64/mebibyte ||
		guestEphemeralDiskMiB > math.MaxInt64/mebibyte {
		return capacity.Vector{}, errors.New("runtime capacity vector is invalid")
	}
	return capacity.Vector{
		CPUMillis:               cpuMillis,
		MemoryBytes:             memoryMiB * mebibyte,
		GuestEphemeralDiskBytes: guestEphemeralDiskMiB * mebibyte,
		VMSlots:                 1,
	}, nil
}
