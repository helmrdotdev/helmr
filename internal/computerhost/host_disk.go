package computerhost

import (
	"errors"
	"math"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
)

// HostDiskPerSlot bounds all files that may coexist for one physical Instance,
// including a restored machine taking another checkpoint. Shared caches and the
// host reserve are withheld separately. Immutable publication uploads these
// files directly; it does not spool another copy.
func HostDiskPerSlot(memoryMiB, scratchMiB, computerStagingBytes int64, cipher *CheckpointEncryptor) (int64, error) {
	if memoryMiB <= 0 || scratchMiB <= 0 || memoryMiB > math.MaxInt64/mebibyte || scratchMiB > math.MaxInt64/mebibyte || computerStagingBytes <= 0 {
		return 0, errors.New("invalid worker lifecycle disk shape")
	}
	memory, scratch := memoryMiB*mebibyte, scratchMiB*mebibyte
	capture, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: disk.SeedCapacity, MemoryBytes: memory, ScratchBytes: scratch, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, cipher)
	if err != nil {
		return 0, err
	}
	state, err := cipher.EncryptedSize(vm.SnapshotStateLimit)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, size := range []int64{scratch, disk.SeedCapacity, computerStagingBytes, artifact.MaxProgramPhysicalBytes, artifact.MaxRuntimePhysicalBytes, memory, state, capture.total} {
		if size > math.MaxInt64-total {
			return 0, reservation.ErrOverflow
		}
		total += size
	}
	return total, nil
}
