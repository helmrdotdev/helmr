package workergroup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
)

const mebibyte = int64(1024 * 1024)

type DemandStore interface {
	ListWorkerDemandObservations(context.Context) ([]db.ListWorkerDemandObservationsRow, error)
}

type ResourceVector struct {
	CPUMillis               int64 `json:"cpu_millis"`
	MemoryBytes             int64 `json:"memory_bytes"`
	GuestEphemeralDiskBytes int64 `json:"guest_ephemeral_disk_bytes"`
	VMSlots                 int64 `json:"vm_slots,omitempty"`
	RunConsumers            int64 `json:"run_consumers,omitempty"`
	BuildExecutors          int64 `json:"build_executors,omitempty"`
}

type RoleDemand struct {
	QueuedItems       int64          `json:"queued_items"`
	QueuedResources   ResourceVector `json:"queued_resources"`
	ReadyWorkers      int64          `json:"ready_workers"`
	AvailableCapacity ResourceVector `json:"available_capacity"`
}

type DemandObservation struct {
	WorkerGroupID      string      `json:"worker_group_id"`
	RegionID           string      `json:"region_id"`
	GroupState         string      `json:"group_state"`
	Run                *RoleDemand `json:"run,omitempty"`
	Build              *RoleDemand `json:"build,omitempty"`
	RegisteringWorkers int64       `json:"registering_workers"`
	DrainingWorkers    int64       `json:"draining_workers"`
	ObservedAt         time.Time   `json:"observed_at"`
}

// ObserveDemand returns advisory deployment telemetry. It does not create a
// transactional capacity snapshot or authority to change physical capacity.
func ObserveDemand(ctx context.Context, store DemandStore) ([]DemandObservation, error) {
	rows, err := store.ListWorkerDemandObservations(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]DemandObservation, 0, len(rows))
	for _, row := range rows {
		if !row.ObservedAt.Valid {
			return nil, errors.New("worker demand observation is missing its observation time")
		}
		observation := DemandObservation{
			WorkerGroupID:      row.WorkerGroupID,
			RegionID:           row.RegionID,
			GroupState:         row.State,
			RegisteringWorkers: row.RegisteringWorkers,
			DrainingWorkers:    row.DrainingWorkers,
			ObservedAt:         row.ObservedAt.Time,
		}
		if row.AllowsRun {
			queued, err := queuedRunResources(row)
			if err != nil {
				return nil, fmt.Errorf("aggregate run demand for worker group %q: %w", row.WorkerGroupID, err)
			}
			observation.Run = &RoleDemand{
				QueuedItems:     row.QueuedRuns,
				QueuedResources: queued,
				ReadyWorkers:    row.ReadyRunWorkers,
				AvailableCapacity: ResourceVector{
					CPUMillis:               row.RunAvailableCPUMillis,
					MemoryBytes:             row.RunAvailableMemoryBytes,
					GuestEphemeralDiskBytes: row.RunAvailableGuestEphemeralDiskBytes,
					VMSlots:                 row.RunAvailableVMSlots,
					RunConsumers:            row.RunAvailableConsumers,
				},
			}
		}
		if row.AllowsBuild {
			build := compute.BuildEnvelopeResources()
			queued, err := queuedResources(row.QueuedBuilds, build.MilliCPU, build.MemoryMiB*mebibyte, build.DiskMiB*mebibyte)
			if err != nil {
				return nil, fmt.Errorf("aggregate build demand for worker group %q: %w", row.WorkerGroupID, err)
			}
			queued.BuildExecutors, err = checkedProduct(row.QueuedBuilds, int64(build.Slots))
			if err != nil {
				return nil, fmt.Errorf("aggregate build executors for worker group %q: %w", row.WorkerGroupID, err)
			}
			observation.Build = &RoleDemand{
				QueuedItems:     row.QueuedBuilds,
				QueuedResources: queued,
				ReadyWorkers:    row.ReadyBuildWorkers,
				AvailableCapacity: ResourceVector{
					CPUMillis:               row.BuildAvailableCPUMillis,
					MemoryBytes:             row.BuildAvailableMemoryBytes,
					GuestEphemeralDiskBytes: row.BuildAvailableGuestEphemeralDiskBytes,
					BuildExecutors:          row.BuildAvailableExecutors,
				},
			}
		}
		result = append(result, observation)
	}
	return result, nil
}

func queuedRunResources(row db.ListWorkerDemandObservationsRow) (ResourceVector, error) {
	if row.QueuedRuns < 0 || row.QueuedRunCPUMillis < 0 || row.QueuedRunMemoryMiB < 0 {
		return ResourceVector{}, errors.New("resource values must be non-negative")
	}
	memory, err := checkedProduct(row.QueuedRunMemoryMiB, mebibyte)
	if err != nil {
		return ResourceVector{}, err
	}
	disk, err := checkedProduct(row.QueuedRuns, compute.WorkspaceGuestEphemeralDiskMiB*mebibyte)
	if err != nil {
		return ResourceVector{}, err
	}
	return ResourceVector{
		CPUMillis: row.QueuedRunCPUMillis, MemoryBytes: memory,
		GuestEphemeralDiskBytes: disk, VMSlots: row.QueuedRuns, RunConsumers: row.QueuedRuns,
	}, nil
}

func queuedResources(items int64, cpuMillis int64, memoryBytes int64, guestDiskBytes int64) (ResourceVector, error) {
	if items < 0 || cpuMillis < 0 || memoryBytes < 0 || guestDiskBytes < 0 {
		return ResourceVector{}, errors.New("resource values must be non-negative")
	}
	cpu, err := checkedProduct(items, cpuMillis)
	if err != nil {
		return ResourceVector{}, err
	}
	memory, err := checkedProduct(items, memoryBytes)
	if err != nil {
		return ResourceVector{}, err
	}
	disk, err := checkedProduct(items, guestDiskBytes)
	if err != nil {
		return ResourceVector{}, err
	}
	return ResourceVector{CPUMillis: cpu, MemoryBytes: memory, GuestEphemeralDiskBytes: disk}, nil
}

func checkedProduct(left int64, right int64) (int64, error) {
	if left < 0 || right < 0 {
		return 0, errors.New("resource values must be non-negative")
	}
	if left != 0 && right > math.MaxInt64/left {
		return 0, errors.New("resource aggregate exceeds int64")
	}
	return left * right, nil
}
