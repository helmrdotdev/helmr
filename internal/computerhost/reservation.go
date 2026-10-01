package computerhost

import (
	"errors"
	"math"
	"strings"

	"github.com/helmrdotdev/helmr/internal/reservation"
)

const mebibyte = int64(1024 * 1024)

func instanceReservationKey(id string, epoch int64) reservation.Key {
	return reservation.Key{Kind: "instance", Epoch: epoch, ID: strings.TrimSpace(id)}
}

func instanceReservationVector(cpuMillis, memoryMiB, guestEphemeralDiskMiB int64) (reservation.Vector, error) {
	if memoryMiB < 0 || guestEphemeralDiskMiB < 0 ||
		memoryMiB > math.MaxInt64/mebibyte ||
		guestEphemeralDiskMiB > math.MaxInt64/mebibyte {
		return reservation.Vector{}, errors.New("instance capacity vector is invalid")
	}
	return reservation.Vector{
		CPUMillis:               cpuMillis,
		MemoryBytes:             memoryMiB * mebibyte,
		GuestEphemeralDiskBytes: guestEphemeralDiskMiB * mebibyte,
		VMSlots:                 1,
	}, nil
}

func instanceReservationVectorWithProjection(
	cpuMillis,
	memoryMiB,
	guestEphemeralDiskMiB,
	projectionBytes int64,
) (reservation.Vector, error) {
	request, err := instanceReservationVector(cpuMillis, memoryMiB, guestEphemeralDiskMiB)
	if err != nil {
		return reservation.Vector{}, err
	}
	if projectionBytes < 0 || request.GuestEphemeralDiskBytes > math.MaxInt64-projectionBytes {
		return reservation.Vector{}, errors.New("instance arena projection capacity is invalid")
	}
	request.GuestEphemeralDiskBytes += projectionBytes
	return request, nil
}
