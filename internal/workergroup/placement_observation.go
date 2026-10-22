package workergroup

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

var ErrHostBusy = errors.New("worker host still owns execution")

// Each pool is a separate projection over the same compatible hosts. Counts are
// not additive across pools and never create demand for additional Workers.
func observePlacements(plans []poolPlan, bins []bin) {
	for i := range plans {
		plan := &plans[i]
		for _, host := range bins {
			shape := plan.template.perVM
			shape.VMSlots = 1
			req := RestoreRequirements{WorkerGroupID: plan.pool.WorkerGroupID, VMPlatformID: plan.pool.VMPlatformID, VCPUCount: int32(shape.CPUMillis / 1000), Resources: shape}
			for _, cpu := range plan.pool.CPUShapes {
				if cpu.VCPUCount == req.VCPUCount {
					req.CPUConfigDigest = cpu.CPUConfigDigest
				}
			}
			if !CanRestore(req, Pool{WorkerGroupID: host.workerGroupID, VMPlatformID: host.vmPlatformID, PerVM: host.perVM, CPUShapes: host.cpuShapes}) {
				continue
			}
			if host.admissionPaused {
				continue
			}
			count := shapeFits(host.resources, shape, host.instanceStarts)
			plan.result.Placements.AfterOptionalReclaim += count
			if host.hostID.Valid {
				plan.result.CreditedHosts = append(plan.result.CreditedHosts, CreditedHost{WorkerHostID: pgvalue.UUIDString(host.hostID), WorkerEpoch: host.epoch})
			}
			if host.immediateKnown {
				if plan.result.Placements.Immediate != nil {
					*plan.result.Placements.Immediate += shapeFits(host.immediate, shape, host.immediateStarts)
				}
				if o := plan.result.OptionalOccupancy; o != nil {
					o.CPUMillis += host.optional.CPUMillis
					o.MemoryBytes += host.optional.MemoryBytes
					o.DiskBytes += host.optional.DiskBytes
					o.VMSlots += host.optional.VMSlots
					o.Tasks += host.optional.Tasks
				}
			} else {
				plan.result.Placements.Immediate = nil
				plan.result.OptionalOccupancy = nil
				plan.result.Complete = false
			}
			if host.reclaimPending {
				plan.result.ReclaimingWorkers++
			}
			plan.result.LocalHeadroomSlotsInUse += max(0, min(int64(host.headroom), host.mandatorySlots-(int64(host.maxSlots)-int64(host.headroom))))
		}
	}
}

func shapeFits(available, shape ResourceVector, starts int64) int64 {
	if shape.CPUMillis <= 0 || shape.MemoryBytes <= 0 || shape.GuestEphemeralDiskBytes <= 0 {
		return 0
	}
	return max(0, min(available.CPUMillis/shape.CPUMillis, available.MemoryBytes/shape.MemoryBytes, available.GuestEphemeralDiskBytes/shape.GuestEphemeralDiskBytes, available.VMSlots, starts))
}
