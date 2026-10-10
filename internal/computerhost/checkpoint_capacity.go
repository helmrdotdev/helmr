package computerhost

import (
	"errors"
	"math"

	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type checkpointStagingLimits struct {
	total   int64
	memory  int64
	scratch int64
	state   int64
	config  int64
}

func checkpointStagingSize(shape vm.SnapshotLimits, cipher *CheckpointEncryptor) (checkpointStagingLimits, error) {
	if shape.ComputerBytes <= 0 || shape.MemoryBytes <= 0 || shape.ScratchBytes <= 0 || shape.StateBytes <= 0 || shape.ConfigBytes <= 0 {
		return checkpointStagingLimits{}, errors.New("incomplete checkpoint capture limits")
	}
	memory, err := filepack.PackedSizeLimit(shape.MemoryBytes, filepack.MemoryRole)
	if err != nil {
		return checkpointStagingLimits{}, err
	}
	scratch, err := filepack.PackedSizeLimit(shape.ScratchBytes, filepack.ScratchRole)
	if err != nil {
		return checkpointStagingLimits{}, err
	}
	limits := checkpointStagingLimits{memory: memory, scratch: scratch, state: shape.StateBytes, config: shape.ConfigBytes}
	// Working Computer and scratch are already charged to the instance. Raw RAM,
	// raw state, packed intermediates and all ciphertexts may coexist here.
	sizes := []int64{shape.MemoryBytes, shape.StateBytes, memory, scratch}
	for _, n := range []int64{memory, scratch, shape.StateBytes, shape.ConfigBytes} {
		encoded, err := cipher.EncryptedSize(n)
		if err != nil {
			return checkpointStagingLimits{}, err
		}
		sizes = append(sizes, encoded)
	}
	for _, n := range sizes {
		if n > math.MaxInt64-limits.total {
			return checkpointStagingLimits{}, reservation.ErrOverflow
		}
		limits.total += n
	}
	return limits, nil
}
