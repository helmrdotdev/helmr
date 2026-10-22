package computerhost

import (
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"math"
)

func (p *PreparedMachines) computerReservation(shape workerapi.AllocationShape, source workerapi.ComputerAllocationSource, program *computercheckpoint.Program) (reservation.Vector, error) {
	request, err := instanceReservationVectorWithProjection(shape.CPUMillis, shape.MemoryBytes/mebibyte, shape.ScratchBytes/mebibyte, source.Disk.Root.LogicalBytes)
	if err != nil {
		return reservation.Vector{}, err
	}
	if request.HostDiskBytes > math.MaxInt64-p.ComputerStagingBytes {
		return reservation.Vector{}, reservation.ErrOverflow
	}
	request.HostDiskBytes += p.ComputerStagingBytes
	limits, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: source.Disk.Root.LogicalBytes, MemoryBytes: shape.MemoryBytes, ScratchBytes: shape.ScratchBytes, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, p.CheckpointCipher)
	if err != nil {
		return reservation.Vector{}, err
	}
	state, err := p.CheckpointCipher.EncryptedSize(vm.SnapshotStateLimit)
	if err != nil {
		return reservation.Vector{}, err
	}
	for _, extra := range []int64{limits.total, shape.MemoryBytes, state, program.Size()} {
		if request.HostDiskBytes > math.MaxInt64-extra {
			return reservation.Vector{}, reservation.ErrOverflow
		}
		request.HostDiskBytes += extra
	}

	return request, nil
}
